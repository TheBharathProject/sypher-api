package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/jobtracker"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// Stale threshold: an application with no stage change in this many days
// gets the `stale=true` flag and a notification on the next 03:00 run.
// The user can clear it just by updating the application — the next
// MarkStaleApplications run won't re-flag a fresh row.
const staleAfter = 14 * 24 * time.Hour

// Deadline window: deadline_soon notifications fire when the apply_deadline
// is anywhere from today through today+deadlineWindow. The digest groups
// stale + deadline-soon items into one daily email.
const deadlineWindow = 3 // days

// notifierForJobs is the subset of Notifier the cron jobs depend on. We
// keep it narrow so jobs are easy to mock in tests, and so a future
// expansion of the real Notifier interface doesn't ripple through here.
type notifierForJobs interface {
	PushIdempotent(ctx context.Context, in jobtracker.NotificationInput, dayKey time.Time) (created bool, err error)
}

// staleStore is the subset of *jobtracker.Store the stale job needs.
type staleStore interface {
	MarkApplicationsStale(ctx context.Context, threshold time.Time) ([]jobtracker.StaleApplication, error)
}

// digestStore is the subset of *jobtracker.Store the digest job needs.
type digestStore interface {
	UsersForDigest(ctx context.Context, today time.Time, deadlineWindowDays int) ([]jobtracker.DigestUser, error)
}

// publicURLBuilder lets a job ship absolute links into emails. It reads
// from the same config helper handlers_profile.go uses, so emails always
// match the apex /u/ + /pegasus paths the user clicks in the frontend.
type publicURLBuilder interface {
	AppLink(applicationID string) string
	DashboardLink() string
}

// MarkStaleApplications runs daily at 03:00 IST. It flips `stale=true` on
// applications whose stage hasn't moved in `staleAfter` days, and pushes a
// per-app `app_stale` notification to the user. Idempotent within a UTC
// day: re-running it doesn't create duplicate notifications because of
// the partial unique index on (user_id, kind, ref_id, day).
//
// Errors per row are logged but don't abort the job — one bad row
// shouldn't stop the rest of the day's nudges.
func MarkStaleApplications(store staleStore, n notifierForJobs, urls publicURLBuilder, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		threshold := time.Now().Add(-staleAfter)
		flipped, err := store.MarkApplicationsStale(ctx, threshold)
		if err != nil {
			return fmt.Errorf("mark stale: %w", err)
		}
		logger.Info("cron stale: flipped", "count", len(flipped))

		dayKey := time.Now().UTC()
		for _, app := range flipped {
			refID := app.ID
			in := jobtracker.NotificationInput{
				UserID:   app.UserID,
				Kind:     "app_stale",
				RefType:  strPtr("application"),
				RefID:    &refID,
				Title:    fmt.Sprintf("%s · %s has gone quiet", app.Company, app.Role),
				Body:     fmt.Sprintf("No movement in %d days — worth a follow-up?", int(staleAfter.Hours()/24)),
				LinkPath: strPtr(urls.AppLink(app.ID.String())),
			}
			if _, err := n.PushIdempotent(ctx, in, dayKey); err != nil {
				logger.Error("cron stale: notify failed", "app_id", app.ID, "err", err)
				// keep going — one user's error doesn't stop the rest
			}
		}
		return nil
	}
}

// DailyApplicationDigest runs at 09:00 IST. For each user with at least
// one stale or deadline-approaching application, it:
//
//  1. Inserts a single `kind='digest'` notification (idempotent: one per
//     user per UTC day, enforced by the partial unique index).
//  2. Sends the digest email through the supplied Mailer. Mailer is
//     responsible for being a no-op if RESEND_API_KEY is unset.
//
// Notifications and emails are independent — the row lands even if the
// email send fails.
func DailyApplicationDigest(
	store digestStore,
	n notifierForJobs,
	m mailer.Mailer,
	urls publicURLBuilder,
	logger *slog.Logger,
) func(context.Context) error {
	return func(ctx context.Context) error {
		today := time.Now().In(IST).Truncate(24 * time.Hour)
		users, err := store.UsersForDigest(ctx, today, deadlineWindow)
		if err != nil {
			return fmt.Errorf("users for digest: %w", err)
		}
		logger.Info("cron digest: candidates", "users", len(users))

		dayKey := time.Now().UTC()
		dashboardURL := urls.DashboardLink()

		for _, u := range users {
			if len(u.Items) == 0 {
				continue
			}

			// 1. In-app notification, idempotent per user per day.
			refID := u.UserID
			in := jobtracker.NotificationInput{
				UserID:   u.UserID,
				Kind:     "digest",
				RefType:  strPtr("user"),
				RefID:    &refID, // ref_id = user_id is intentional — one digest per user
				Title:    fmt.Sprintf("%d application%s could use a look", len(u.Items), plural(len(u.Items))),
				Body:     digestPreview(u.Items),
				LinkPath: strPtr("/applications"),
			}
			if _, err := n.PushIdempotent(ctx, in, dayKey); err != nil {
				logger.Error("cron digest: notify failed", "user_id", u.UserID, "err", err)
				continue
			}

			// 2. Email — gated. Free users always get the in-app row above;
			// only premium users with the opt-in toggle on get the email.
			// See docs/adr/0002-premium-email-gating.md (D5).
			//
			// CanReceiveEmail is computed in SQL inside UsersForDigest so
			// we don't round-trip again here. Empty Email is the same shape
			// of skip (would never happen for a Google-OAuth user, but
			// defensive nil-check).
			if u.Email == "" || !u.CanReceiveEmail {
				if !u.CanReceiveEmail {
					logger.Info("cron digest: email skipped (not premium or opted out)", "user_id", u.UserID)
				}
				continue
			}
			items := make([]mailer.DigestItem, 0, len(u.Items))
			for _, it := range u.Items {
				items = append(items, mailer.DigestItem{
					Company: it.Company,
					Role:    it.Role,
					Reason:  it.Reason,
					Detail:  it.Detail,
					URL:     urls.AppLink(it.AppID.String()),
				})
			}
			subject, html, text, rerr := mailer.RenderDigest(mailer.DigestData{
				UserName:     u.Name,
				Items:        items,
				DashboardURL: dashboardURL,
			})
			if rerr != nil {
				logger.Error("cron digest: render failed", "user_id", u.UserID, "err", rerr)
				continue
			}
			if err := m.Send(ctx, mailer.Message{
				To:      u.Email,
				Subject: subject,
				HTML:    html,
				Text:    text,
			}); err != nil {
				logger.Error("cron digest: email failed", "user_id", u.UserID, "err", err)
				// keep going — D6: in-app notification already landed
			}
		}
		return nil
	}
}

// digestPreview returns a one-sentence summary of the digest for the
// in-app notification body. Email gets the full list; the bell badge
// just needs a glanceable line.
func digestPreview(items []jobtracker.DigestItem) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return fmt.Sprintf("%s · %s", items[0].Company, items[0].Role)
	}
	companies := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.Company] {
			continue
		}
		seen[it.Company] = true
		companies = append(companies, it.Company)
		if len(companies) == 3 {
			break
		}
	}
	if len(items) > len(companies) {
		return strings.Join(companies, ", ") + " and others"
	}
	return strings.Join(companies, ", ")
}

func strPtr(s string) *string { return &s }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
