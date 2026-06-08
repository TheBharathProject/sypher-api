package provider

import "errors"

// Typed errors the rest of the kairos package can compare against with
// errors.Is. Callers should not parse strings to decide what happened.

// ErrAuthExpired means the provider's auth state (e.g. Kite daily access
// token) is missing, expired, or rejected by the provider. The cron job
// uses this to skip cleanly without crashing the loop; the /provider/status
// handler reports this back to the FE so the user can re-auth.
var ErrAuthExpired = errors.New("provider: auth expired or missing")

// ErrNotImplemented is returned by stub providers (dhan, upstox, angel
// before they're wired up) so the system degrades gracefully — a cron
// fires, the call returns ErrNotImplemented, the cron logs and moves on.
// Never crash.
var ErrNotImplemented = errors.New("provider: not implemented")

// ErrRateLimited is returned when the upstream provider says we're going
// too fast. The cron job logs and retries on the next tick; the backtest
// worker either retries with backoff or fails the job with this error
// stored in backtests.error.
var ErrRateLimited = errors.New("provider: rate limited")

// ErrNotConfigured indicates the provider was selected (KAIROS_DATA_PROVIDER
// is set to its name) but its required env vars (e.g. KAIROS_KITE_API_KEY)
// are blank. Distinct from ErrAuthExpired: this is a deployment misstep,
// not a runtime token refresh.
var ErrNotConfigured = errors.New("provider: not configured")

// ErrUnknownUnderlying is returned for symbols this provider hasn't been
// taught about. Different providers support different sets — most
// support NIFTY/BANKNIFTY/SENSEX, but for new additions this is the
// signal that translate.go needs a new mapping.
var ErrUnknownUnderlying = errors.New("provider: unknown underlying")
