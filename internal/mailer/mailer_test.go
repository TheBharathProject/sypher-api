package mailer

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/TheBharathProject/sypher-api/internal/config"
)

// silentLogger is a slog discard sink for tests. Mailer.New logs at
// Info on configure/skip — we don't want test output cluttered.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewChoosesImpl(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		// The boolean is "is the returned value a *resendMailer?". When
		// false we expect a *slogMailer fallback.
		isResend bool
	}{
		{
			name:     "nil cfg → slog fallback",
			cfg:      nil,
			isResend: false,
		},
		{
			name:     "empty key → slog fallback",
			cfg:      &config.Config{},
			isResend: false,
		},
		{
			name:     "key set but no from address → slog fallback",
			cfg:      &config.Config{ResendAPIKey: "re_xxx"},
			isResend: false,
		},
		{
			name: "key + from set → resend impl",
			cfg: &config.Config{
				ResendAPIKey:    "re_xxx",
				MailFromAddress: "hello@example.com",
				MailFromName:    "Pegasus",
			},
			isResend: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(c.cfg, silentLogger())
			_, ok := m.(*resendMailer)
			if ok != c.isResend {
				t.Errorf("New(%+v): got resendMailer=%v, want %v", c.cfg, ok, c.isResend)
			}
		})
	}
}

func TestFormatFrom(t *testing.T) {
	cases := []struct {
		addr string
		name string
		want string
	}{
		{"hello@sypher.in", "", "hello@sypher.in"},
		{"hello@sypher.in", "Pegasus", "Pegasus <hello@sypher.in>"},
		{"hello@sypher.in", "Sypher Studio", "Sypher Studio <hello@sypher.in>"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := formatFrom(c.addr, c.name); got != c.want {
				t.Errorf("formatFrom(%q, %q) = %q, want %q", c.addr, c.name, got, c.want)
			}
		})
	}
}

// SlogMailer.Send always returns nil — it can't fail. Test just confirms
// the contract (no panics on edge inputs) so future-self knows the slog
// fallback is genuinely a no-op even on weird messages.
func TestSlogMailerNeverErrors(t *testing.T) {
	m := &slogMailer{logger: silentLogger()}
	cases := []Message{
		{To: "", Subject: "", HTML: "", Text: ""},
		{To: "a@b.c", Subject: "Hi", HTML: "<p>Hi</p>"},
		{To: "a@b.c", Subject: "Hi", Text: "Hi"},
	}
	for _, c := range cases {
		if err := m.Send(context.Background(), c); err != nil {
			t.Errorf("slogMailer.Send(%+v) returned err: %v", c, err)
		}
	}
}
