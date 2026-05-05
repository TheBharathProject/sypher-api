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

	cfg := &Config{
		DatabaseURL:     required("DATABASE_URL"),
		WaitlistAPIKey:  required("WAITLIST_API_KEY"),
		IPSalt:          envWithDefault("IP_SALT", "change-me-please"),
		CORSOrigins:     splitCSV(envWithDefault("CORS_ORIGINS", "https://sypher.in,https://www.sypher.in")),
		Env:             envWithDefault("ENV", "prod"),
		HTTPListenAddr:  envWithDefault("HTTP_LISTEN_ADDR", ":8000"),
		ShutdownTimeout: shutdown,
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
