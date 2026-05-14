package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/jobtracker"
)

// reminderStore is the subset of *jobtracker.Store the reminders cron needs.
type reminderStore interface {
	DueReminders(ctx context.Context) ([]jobtracker.DueReminder, error)
	MarkReminderFired(ctx context.Context, id uuid.UUID) error
}

// reminderNotifier is the notification push the reminders cron needs.
type reminderNotifier interface {
	Push(ctx context.Context, in jobtracker.NotificationInput) (*jobtracker.Notification, error)
}

// EveryN returns a NextFire function that fires every d interval from now.
// Suitable for sub-daily jobs like reminders (every 5 min).
func EveryN(d time.Duration) func(now time.Time) time.Time {
	return func(now time.Time) time.Time {
		return now.Add(d)
	}
}

// FireDueReminders processes all unfired due reminders: emits a notification
// for each, then marks the row fired. Runs every 5 minutes. Errors per
// reminder are logged but don't abort the loop — one bad row shouldn't
// block all others.
func FireDueReminders(store reminderStore, notifier reminderNotifier, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		due, err := store.DueReminders(ctx)
		if err != nil {
			return fmt.Errorf("fire reminders: %w", err)
		}
		for _, r := range due {
			refType := "application"
			refID := r.ApplicationID
			body := ""
			if r.Note != nil {
				body = *r.Note
			}
			_, nerr := notifier.Push(ctx, jobtracker.NotificationInput{
				UserID:   r.UserID,
				Kind:     "reminder",
				RefType:  &refType,
				RefID:    &refID,
				Title:    "Reminder",
				Body:     body,
				LinkPath: linkPath(r.ApplicationID),
			})
			if nerr != nil {
				logger.Error("reminders: push notification failed", "id", r.ID, "err", nerr)
				continue
			}
			if merr := store.MarkReminderFired(ctx, r.ID); merr != nil {
				logger.Error("reminders: mark fired failed", "id", r.ID, "err", merr)
			}
		}
		if len(due) > 0 {
			logger.Info("reminders: fired", "count", len(due))
		}
		return nil
	}
}

// linkPath returns a pointer to the application link path for in-app nav.
// basePath-relative (no `/pegasus` prefix) — the FE's router.push()
// prepends basePath automatically. The FE applications page reads
// `?view=<id>` to auto-open the detail panel (see job-tracker/app/
// applications/page.tsx); the redirector at app/applications/[id]/page.tsx
// also rewrites /applications/<id> to the same `?view=` form.
func linkPath(appID uuid.UUID) *string {
	s := fmt.Sprintf("/applications?view=%s", appID)
	return &s
}
