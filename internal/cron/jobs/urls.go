package jobs

import (
	"net/url"
	"strings"

	"github.com/TheBharathProject/sypher-api/internal/config"
)

// URLBuilder produces absolute, click-through URLs for inclusion in
// emails. We build off `cfg.FrontendLoginRedirect` because it's the one
// env var that's guaranteed to know the apex host (sypher.in or
// localhost:3000) — same approach as handlers_profile.go's
// publicProfileBase.
//
// Pegasus lives at <apex>/pegasus/* via the shell rewrite, so application
// links resolve to <apex>/pegasus/applications/<id>. The prefix is
// hardcoded — when tool #2 starts using sypher-api, we'll either extract
// this to a tool-aware builder or pass the prefix in via config.
//
// See ADR-001 D8 (D6 from the plan) on FRONTEND_LOGIN_REDIRECT_URL being
// the single source of truth for "where is the frontend".
type URLBuilder struct {
	apex string // "https://sypher.in" or "http://localhost:3000"
}

const pegasusBasePath = "/pegasus"

func NewURLBuilder(cfg *config.Config) *URLBuilder {
	apex := apexFromConfig(cfg)
	return &URLBuilder{apex: apex}
}

// AppLink returns the absolute URL for an application's detail view.
// Application detail is rendered as a modal on the /applications page;
// linking there gets the user one click closer than a generic dashboard.
func (b *URLBuilder) AppLink(appID string) string {
	return b.apex + pegasusBasePath + "/applications#" + appID
}

// DashboardLink is the "Open Pegasus" CTA target used by digest emails.
func (b *URLBuilder) DashboardLink() string {
	return b.apex + pegasusBasePath + "/dashboard"
}

// apexFromConfig pulls the scheme + host off FrontendLoginRedirect.
//
//	"https://sypher.in/pegasus/auth/callback" → "https://sypher.in"
//	"http://localhost:3000/pegasus/auth/callback" → "http://localhost:3000"
//
// If parsing fails we fall back to a sensible prod default rather than
// produce broken email links.
func apexFromConfig(cfg *config.Config) string {
	const fallback = "https://sypher.in"
	if cfg == nil || cfg.FrontendLoginRedirect == "" {
		return fallback
	}
	u, err := url.Parse(cfg.FrontendLoginRedirect)
	if err != nil || u.Host == "" {
		return fallback
	}
	apex := u.Scheme + "://" + u.Host
	return strings.TrimRight(apex, "/")
}
