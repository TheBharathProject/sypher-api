package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// expireOneTimeStore is the subset of *billing.Store the expiry job
// needs. Kept narrow so the cron tests can mock without dragging the
// full store in.
type expireOneTimeStore interface {
	ExpireOldOneTime(ctx context.Context) ([]uuid.UUID, error)
	RefreshUserPremium(ctx context.Context, userID uuid.UUID) error
}

// ExpireOneTimePremium runs daily at 04:00 IST. One-time premium passes
// don't have a renewal webhook (Razorpay only sends payment.captured at
// purchase time), so this is the only mechanism that flips them from
// 'active' to 'expired' once their 30-day window passes.
//
// For each affected row we also re-derive auth.users.is_premium so the
// email-gate downstream sees the change without a separate refresh.
//
// Errors per row are logged but don't abort — one bad refresh shouldn't
// stop the rest.
func ExpireOneTimePremium(store expireOneTimeStore, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		expired, err := store.ExpireOldOneTime(ctx)
		if err != nil {
			return fmt.Errorf("expire one_time: %w", err)
		}
		logger.Info("cron expire-one-time-premium: expired", "count", len(expired))
		for _, uid := range expired {
			if err := store.RefreshUserPremium(ctx, uid); err != nil {
				logger.Error("cron refresh premium", "user_id", uid, "err", err)
			}
		}
		return nil
	}
}
