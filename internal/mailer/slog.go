package mailer

import (
	"context"
	"log/slog"
)

// slogMailer satisfies Mailer without sending anything. Useful in:
//
//   - Local dev where we don't want to spam ourselves with test emails.
//   - Tests, which never reach the network.
//   - Prod boots where RESEND_API_KEY is unset (e.g. before DKIM
//     verification clears). The notification row still gets written; only
//     the email leg is no-op'd.
//
// Logs a single line per send so the would-be recipient + subject is
// visible in journalctl. We don't log the body because templates can be
// long and Subject is enough to confirm the right call site fired.
type slogMailer struct {
	logger *slog.Logger
}

func (m *slogMailer) Send(_ context.Context, in Message) error {
	m.logger.Info("mailer (no-op)", "to", in.To, "subject", in.Subject)
	return nil
}
