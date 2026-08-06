// Package config loads runtime configuration from environment variables.
//
// Why this is its own package, not just inline os.Getenv calls in main.go:
//   - "fail fast" — Load() returns an error if a required var is missing,
//     so the binary refuses to start instead of crashing on the first request
//   - centralised — every place that needs config imports this; no scattered
//     os.Getenv calls hiding what we depend on
//   - typed — callers receive a Config struct, not a stringly-typed map
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the parsed-and-validated runtime configuration.
//
// Note that fields are exported (Capitalised) — Go's visibility rules use
// case: lowercase = package-private, Uppercase = exported. The struct is
// returned to main(), so the fields must be exported.
type Config struct {
	DatabaseURL     string
	WaitlistAPIKey  string
	IPSalt          string
	CORSOrigins     []string
	Env             string        // "dev" | "prod"
	HTTPListenAddr  string        // ":8000" inside the container
	ShutdownTimeout time.Duration // wait this long for in-flight requests on SIGTERM

	// Auth — Google OAuth + JWT (used by internal/auth).
	GoogleOAuthClientID     string
	GoogleOAuthClientSecret string
	GoogleOAuthRedirectURL  string
	JWTSecret               string
	JWTIssuer               string
	JWTAudience             string
	JWTTTL                  time.Duration
	FrontendLoginRedirect   string
	// Per-tool redirect overrides. If set, the OAuth callback sends users
	// of that tool to the tool-specific URL instead of FrontendLoginRedirect.
	// Optional — falls back to FrontendLoginRedirect when blank.
	KairosFrontendLoginRedirect string

	// Public profile URL prefix the API uses when echoing /u/<slug> URLs back
	// to clients (Settings, Open Graph, etc.). Split out from
	// FrontendLoginRedirect so that adding a second tool doesn't accidentally
	// move the apex /u/ path under one tool's basePath. Defaults to
	// derive-from-FrontendLoginRedirect for local dev convenience.
	PublicProfileBaseURL    string

	// Storage — Cloudflare R2 (used by internal/storage). Optional in Phase 1.
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2PublicURL       string

	// Email — Resend (used by internal/mailer). All optional. If
	// ResendAPIKey or MailFromAddress is empty, the mailer falls back to
	// a no-op slog implementation and Phase 2 still ships in-app
	// notifications. See docs/adr/0001-notifications-email-cron.md (D2).
	ResendAPIKey    string
	MailFromAddress string
	MailFromName    string

	// AI — Deepseek (used by internal/ai). Optional in Phase 1.
	DeepseekAPIKey            string
	DeepseekBaseURL           string
	DeepseekModel             string
	AIUsageMonthlyTokenLimit  int64

	// Razorpay — payments (used by internal/billing). Optional; if any
	// of RazorpayKeyID/Secret are blank, the billing handlers respond
	// 503 service_unavailable instead of refusing to boot. The webhook
	// secret is set per-endpoint in the Razorpay dashboard. PlanID is
	// the recurring-monthly plan, created once in the dashboard and
	// referenced when starting subscriptions. See ADR-0006 (D8).
	RazorpayKeyID         string
	RazorpayKeySecret     string
	RazorpayWebhookSecret string
	// Razorpay Plan id for the standard ₹99/mo tier (premium-only).
	RazorpayPlanID        string
	// Razorpay Plan id for the ₹299/mo tier (premium + 200 credits per
	// charge). Optional — when blank, the /billing/checkout/subscription-
	// plus endpoint responds 503 service_unavailable and the frontend's
	// Premium+ card is hidden.
	RazorpayPlanIDPlus    string

	// Slack incoming-webhook URL for feedback delivery. Optional — when
	// blank, feedback is persisted to job_tracker.feedback only (the
	// existing behaviour). When set, every successful feedback POST
	// also fans out a short message to this webhook so we see ideas /
	// bug reports in real time without polling the DB.
	SlackFeedbackWebhookURL string

	// LaTeX compile service (used by internal/jobtracker/resume_builder_render).
	// Points at a sidecar container running yotech/latex-on-http (or
	// compatible) — Resume Builder PDF exports POST .tex to this URL and
	// receive PDF bytes back. Optional: when blank, render endpoints
	// respond 503 latex_service_unavailable and the FE falls back to
	// showing the raw .tex source. Set to "http://sypher-tex" in prod
	// (Docker network alias) or "http://localhost:8090" in dev.
	LatexServiceURL string

	// Kairos — options research tool (second product). See ADR-0011 for
	// the provider-abstraction architecture and ADR-0014 for the
	// storage boundary.
	//
	// KairosDataProvider names the active data feed (env:
	// KAIROS_DATA_PROVIDER). Valid values: "kite" (default), "dhan",
	// "upstox", "angel", "null". Each provider reads its own
	// per-provider keys from the env; only the active provider needs
	// its keys set. Switching is config-only (ADR-0011 D3) — restart
	// the binary after changing.
	KairosDataProvider string

	// Kite Connect (₹500/mo). Access token is exchanged from a daily
	// OAuth request_token and persisted in kairos.provider_tokens; the
	// env vars below are the long-lived API key + secret only.
	KairosKiteAPIKey    string
	KairosKiteAPISecret string

	// Other providers — env vars declared so a switch is config-only.
	// Dhan: long-lived access token + client ID from developer.dhan.co.
	KairosDhanAccessToken string
	KairosDhanClientID    string

	KairosUpstoxClientID     string
	KairosUpstoxClientSecret string

	KairosAngelAPIKey      string
	KairosAngelClientCode  string
	KairosAngelPassword    string
	KairosAngelTOTPSecret  string

	// KairosRetentionWeeks bounds how far back the partition-prune cron
	// keeps option_chains data. Defaults to 156 weeks (~3 years).
	KairosRetentionWeeks int64

	// KairosLiveTrading is the dark-launch kill switch for real order
	// execution (env: KAIROS_LIVE_TRADING, default off). While off,
	// POST /kairos/orders refuses with 403 execution_disabled; paper
	// trading is unaffected. Accepted truthy values: "1", "true", "on",
	// "yes" (case-insensitive) — anything else, including unset, is off.
	KairosLiveTrading bool
}

// Load reads the environment and returns a Config or an error explaining
// what's missing. It does not panic; the caller decides what to do.
//
// Convention: required vars are looked up explicitly; optional vars use
// envWithDefault.
func Load() (*Config, error) {
	var missing []string
	required := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}

	shutdown, err := time.ParseDuration(envWithDefault("SHUTDOWN_TIMEOUT", "15s"))
	if err != nil {
		return nil, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
	}

	jwtTTL, err := time.ParseDuration(envWithDefault("JWT_TTL", "168h"))
	if err != nil {
		return nil, fmt.Errorf("JWT_TTL: %w", err)
	}

	// 25k tokens is the free monthly quota. Beyond this, AI calls fall
	// back to the paid credits balance — see internal/billing/costs.go
	// for per-operation pricing.
	aiLimit, err := parseInt64(envWithDefault("AI_USAGE_MONTHLY_TOKEN_LIMIT", "25000"))
	if err != nil {
		return nil, fmt.Errorf("AI_USAGE_MONTHLY_TOKEN_LIMIT: %w", err)
	}

	// Kairos partition retention. 156 weeks ≈ 3 years (ADR-0009 D5).
	kairosRetention, err := parseInt64(envWithDefault("KAIROS_RETENTION_WEEKS", "156"))
	if err != nil {
		return nil, fmt.Errorf("KAIROS_RETENTION_WEEKS: %w", err)
	}

	cfg := &Config{
		DatabaseURL:     required("DATABASE_URL"),
		WaitlistAPIKey:  required("WAITLIST_API_KEY"),
		IPSalt:          envWithDefault("IP_SALT", "change-me-please"),
		// Localhost ports: 3000 = pegasus dev, 3001 = kairos dev. Both
		// need to talk to api:8000 in dev.
		CORSOrigins:     splitCSV(envWithDefault("CORS_ORIGINS", "https://sypher.in,https://www.sypher.in,http://localhost:3000,http://localhost:3001")),
		Env:             envWithDefault("ENV", "prod"),
		HTTPListenAddr:  envWithDefault("HTTP_LISTEN_ADDR", ":8000"),
		ShutdownTimeout: shutdown,

		GoogleOAuthClientID:     required("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleOAuthClientSecret: required("GOOGLE_OAUTH_CLIENT_SECRET"),
		GoogleOAuthRedirectURL:  required("GOOGLE_OAUTH_REDIRECT_URL"),
		JWTSecret:               required("JWT_SECRET"),
		JWTIssuer:               envWithDefault("JWT_ISSUER", "sypher.in"),
		JWTAudience:             envWithDefault("JWT_AUDIENCE", "sypher.in"),
		JWTTTL:                  jwtTTL,
		FrontendLoginRedirect:        required("FRONTEND_LOGIN_REDIRECT_URL"),
		KairosFrontendLoginRedirect: os.Getenv("KAIROS_FRONTEND_LOGIN_REDIRECT_URL"),
		// Default deliberately empty — handlers fall back to derive-from-
		// FrontendLoginRedirect so existing dev setups keep working without
		// touching .env. Set explicitly in prod to "https://sypher.in/u/".
		PublicProfileBaseURL:    envWithDefault("PUBLIC_PROFILE_BASE_URL", ""),

		// Email — all optional. Mailer.New degrades to slog noop if blank.
		ResendAPIKey:    os.Getenv("RESEND_API_KEY"),
		MailFromAddress: os.Getenv("MAIL_FROM_ADDRESS"),
		MailFromName:    envWithDefault("MAIL_FROM_NAME", "Pegasus"),

		// Optional in Phase 1 — only required when Phase 2 endpoints are hit.
		R2AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:          os.Getenv("R2_BUCKET"),
		R2PublicURL:       os.Getenv("R2_PUBLIC_URL"),

		DeepseekAPIKey:           os.Getenv("DEEPSEEK_API_KEY"),
		DeepseekBaseURL:          envWithDefault("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"),
		DeepseekModel:            envWithDefault("DEEPSEEK_MODEL", "deepseek-chat"),
		AIUsageMonthlyTokenLimit: aiLimit,

		RazorpayKeyID:         os.Getenv("RAZORPAY_KEY_ID"),
		RazorpayKeySecret:     os.Getenv("RAZORPAY_KEY_SECRET"),
		RazorpayWebhookSecret: os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
		RazorpayPlanID:        os.Getenv("RAZORPAY_PLAN_ID"),
		RazorpayPlanIDPlus:    os.Getenv("RAZORPAY_PLAN_ID_PLUS"),

		SlackFeedbackWebhookURL: os.Getenv("SLACK_FEEDBACK_WEBHOOK_URL"),

		// Sidecar URL for the LaTeX compile service. Empty = Resume
		// Builder PDF endpoints return 503; FE falls back to .tex view.
		LatexServiceURL: os.Getenv("LATEX_SERVICE_URL"),

		// Kairos data provider + per-provider keys. Only the active
		// provider needs its keys set; others stay blank.
		KairosDataProvider: envWithDefault("KAIROS_DATA_PROVIDER", "kite"),

		KairosKiteAPIKey:    os.Getenv("KAIROS_KITE_API_KEY"),
		KairosKiteAPISecret: os.Getenv("KAIROS_KITE_API_SECRET"),

		KairosDhanAccessToken: os.Getenv("KAIROS_DHAN_ACCESS_TOKEN"),
		KairosDhanClientID:    os.Getenv("KAIROS_DHAN_CLIENT_ID"),

		KairosUpstoxClientID:     os.Getenv("KAIROS_UPSTOX_CLIENT_ID"),
		KairosUpstoxClientSecret: os.Getenv("KAIROS_UPSTOX_CLIENT_SECRET"),

		KairosAngelAPIKey:     os.Getenv("KAIROS_ANGEL_API_KEY"),
		KairosAngelClientCode: os.Getenv("KAIROS_ANGEL_CLIENT_CODE"),
		KairosAngelPassword:   os.Getenv("KAIROS_ANGEL_PASSWORD"),
		KairosAngelTOTPSecret: os.Getenv("KAIROS_ANGEL_TOTP_SECRET"),

		KairosRetentionWeeks: kairosRetention,

		KairosLiveTrading: envBool("KAIROS_LIVE_TRADING"),
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	if cfg.Env != "dev" && cfg.Env != "prod" {
		return nil, errors.New(`ENV must be "dev" or "prod"`)
	}

	return cfg, nil
}

func envWithDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envBool parses an opt-in boolean env var: "1" / "true" / "on" /
// "yes" (case-insensitive) → true; everything else (including unset)
// → false. Used for dark-launch flags that must default off.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseInt64(s string) (int64, error) {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", s)
		}
		v = v*10 + int64(c-'0')
	}
	return v, nil
}
