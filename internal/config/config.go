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

	aiLimit, err := parseInt64(envWithDefault("AI_USAGE_MONTHLY_TOKEN_LIMIT", "1000000"))
	if err != nil {
		return nil, fmt.Errorf("AI_USAGE_MONTHLY_TOKEN_LIMIT: %w", err)
	}

	cfg := &Config{
		DatabaseURL:     required("DATABASE_URL"),
		WaitlistAPIKey:  required("WAITLIST_API_KEY"),
		IPSalt:          envWithDefault("IP_SALT", "change-me-please"),
		CORSOrigins:     splitCSV(envWithDefault("CORS_ORIGINS", "https://sypher.in,https://www.sypher.in,http://localhost:3000")),
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
		FrontendLoginRedirect:   required("FRONTEND_LOGIN_REDIRECT_URL"),
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
