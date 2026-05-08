package jobtracker

import (
	"context"
	"log/slog"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// Notifier is the bridge between event sources (handlers, cron jobs) and
// the notifications + email side-effects. Every implementation MUST be
// safe to call from a request goroutine and from the cron loop —
// idempotency is delegated to the store layer per ADR-001 D5.
//
// Two methods, deliberately:
//
//   - Push:            "create one notification, no questions asked".
//                       Used by handlers (community comment, future
//                       trigger points). Caller doesn't worry about
//                       duplicates.
//
//   - PushIdempotent:  "create unless one already exists for this
//                       (user, kind, ref, day)". Used by cron jobs that
//                       might double-fire across a deploy. Returns
//                       (created, err) so callers know whether to also
//                       send an email.
type Notifier interface {
	Push(ctx context.Context, in NotificationInput) (*Notification, error)
	PushIdempotent(ctx context.Context, in NotificationInput, dayKey time.Time) (created bool, err error)
}

// notifier is the production impl. Wraps the store + mailer + logger.
//
// Email-send policy:
//   - Push doesn't send email. Today no Push trigger needs email; if
//     Phase 3 community comments want email, this wraps the call site.
//   - Cron (PushIdempotent) sends email separately, after PushIdempotent
//     returns true (= row landed). That keeps email opt-in per call site.
type notifier struct {
	store  *Store
	mailer mailer.Mailer
	logger *slog.Logger
}

// NewNotifier wires the dependencies. mailer is allowed to be nil —
// the slog mailer should be substituted upstream rather than nil here,
// but if a caller forgets, the email leg is just skipped.
func NewNotifier(store *Store, m mailer.Mailer, logger *slog.Logger) Notifier {
	return &notifier{store: store, mailer: m, logger: logger}
}

func (n *notifier) Push(ctx context.Context, in NotificationInput) (*Notification, error) {
	notif, err := n.store.CreateNotification(ctx, in)
	if err != nil {
		return nil, err
	}
	n.logger.Info("notifier: pushed", "user_id", in.UserID, "kind", in.Kind, "ref_id", in.RefID)
	return notif, nil
}

func (n *notifier) PushIdempotent(ctx context.Context, in NotificationInput, dayKey time.Time) (bool, error) {
	_, created, err := n.store.CreateNotificationIdempotent(ctx, in, dayKey)
	if err != nil {
		return false, err
	}
	if created {
		n.logger.Info("notifier: pushed (idempotent)", "user_id", in.UserID, "kind", in.Kind, "ref_id", in.RefID)
	} else {
		n.logger.Info("notifier: deduped", "user_id", in.UserID, "kind", in.Kind, "ref_id", in.RefID)
	}
	return created, nil
}
