// Package mailer ships transactional + digest email out of sypher-api.
//
// Two implementations satisfy the Mailer interface:
//
//   - resendMailer: posts to https://api.resend.com/emails. Used in prod when
//     RESEND_API_KEY is set + the sending domain is DKIM/SPF-verified.
//   - slogMailer:    logs the would-be send and returns nil. Used in dev,
//                    in tests, and any prod boot where RESEND_API_KEY is
//                    unset (graceful degradation per ADR-001 D6).
//
// New() picks the right impl based on config — callers don't care which
// one they get. There's no SDK; resendMailer is ~40 lines of net/http.
//
// See docs/adr/0001-notifications-email-cron.md decisions D2, D6, D7, D9.
package mailer

import (
	"context"
	"log/slog"

	"github.com/TheBharathProject/sypher-api/internal/config"
)

// Mailer sends a single transactional email. Implementations must accept a
// canceled context and bail out within a short timeout — callers fire-and-
// forget from goroutines whose lifetime is bounded by a 10s context.
type Mailer interface {
	Send(ctx context.Context, in Message) error
}

// Message is the subset of email fields we need today. Add Cc/Bcc/Reply-To
// fields here if/when the product needs them — keep the surface narrow.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// New returns a Mailer chosen by config. Never errors — a missing API key
// falls back to slogMailer and the rest of the system carries on. Callers
// can compare the returned implementation type if they really need to know
// (e.g. for a /health-style readiness probe), but typical code shouldn't.
func New(cfg *config.Config, logger *slog.Logger) Mailer {
	if cfg == nil || cfg.ResendAPIKey == "" {
		logger.Info("mailer disabled (no RESEND_API_KEY); using slog mailer")
		return &slogMailer{logger: logger}
	}
	from := cfg.MailFromAddress
	if from == "" {
		// Resend rejects sends with no from. If we got here with an API key
		// but no MAIL_FROM_ADDRESS, fall back to slog rather than fail every
		// send at runtime.
		logger.Warn("mailer disabled (RESEND_API_KEY set but MAIL_FROM_ADDRESS empty); using slog mailer")
		return &slogMailer{logger: logger}
	}
	logger.Info("mailer configured (resend)", "from", from)
	return &resendMailer{
		apiKey:   cfg.ResendAPIKey,
		fromAddr: from,
		fromName: cfg.MailFromName,
		logger:   logger,
	}
}

// FromHeader returns "Name <addr>" if the name is set, else the bare addr.
// Resend accepts both formats; we use the named form when we have one so
// inboxes show "Pegasus <hello@sypher.in>" instead of just the address.
func formatFrom(addr, name string) string {
	if name == "" {
		return addr
	}
	return name + " <" + addr + ">"
}
