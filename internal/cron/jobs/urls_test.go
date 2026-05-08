package jobs

import (
	"strings"
	"testing"

	"github.com/TheBharathProject/sypher-api/internal/config"
)

func TestApexFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "nil config — fallback to prod",
			cfg:  nil,
			want: "https://sypher.in",
		},
		{
			name: "empty redirect — fallback to prod",
			cfg:  &config.Config{},
			want: "https://sypher.in",
		},
		{
			name: "prod redirect",
			cfg:  &config.Config{FrontendLoginRedirect: "https://sypher.in/pegasus/auth/callback"},
			want: "https://sypher.in",
		},
		{
			name: "localhost dev redirect",
			cfg:  &config.Config{FrontendLoginRedirect: "http://localhost:3000/pegasus/auth/callback"},
			want: "http://localhost:3000",
		},
		{
			name: "preserves scheme + port",
			cfg:  &config.Config{FrontendLoginRedirect: "https://staging.sypher.in:8443/pegasus/auth/callback"},
			want: "https://staging.sypher.in:8443",
		},
		{
			name: "garbage redirect — fallback to prod (parse fails on missing scheme)",
			cfg:  &config.Config{FrontendLoginRedirect: "not a url"},
			want: "https://sypher.in",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := apexFromConfig(c.cfg)
			if got != c.want {
				t.Errorf("apexFromConfig(%+v) = %q, want %q", c.cfg, got, c.want)
			}
		})
	}
}

func TestURLBuilder(t *testing.T) {
	b := NewURLBuilder(&config.Config{
		FrontendLoginRedirect: "https://sypher.in/pegasus/auth/callback",
	})

	// AppLink: includes /pegasus/applications + the id as a hash
	// fragment. The hash form means the application detail modal opens
	// directly when the email click lands on the page.
	app := b.AppLink("abc-123")
	wantApp := "https://sypher.in/pegasus/applications#abc-123"
	if app != wantApp {
		t.Errorf("AppLink = %q, want %q", app, wantApp)
	}

	dash := b.DashboardLink()
	wantDash := "https://sypher.in/pegasus/dashboard"
	if dash != wantDash {
		t.Errorf("DashboardLink = %q, want %q", dash, wantDash)
	}

	// Sanity: every link starts with https:// in prod. Cron emails are
	// rendered server-side — relative URLs would render as broken links
	// once they reach an inbox.
	if !strings.HasPrefix(app, "https://") {
		t.Errorf("AppLink should be absolute, got %q", app)
	}
	if !strings.HasPrefix(dash, "https://") {
		t.Errorf("DashboardLink should be absolute, got %q", dash)
	}
}
