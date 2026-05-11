package jobtracker

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/ai"
	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/billing"
	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/storage"
)

// Handler bundles the dependencies every /job-tracker/* endpoint needs.
// r2/ai/aiUsage/notifier are optional — Phase-1-only deployments leave
// them nil and the corresponding endpoints respond 503 (or skip the
// side-effect, as with notifier).
type Handler struct {
	cfg          *config.Config
	store        *Store
	authStore    *auth.Store
	logger       *slog.Logger
	r2           *storage.R2
	ai           *ai.Client
	aiUsage      *ai.UsageStore
	notifier     Notifier
	billingStore *billing.Store // nil when billing isn't configured; AI fallback to credits is skipped
}

func NewHandler(cfg *config.Config, store *Store, authStore *auth.Store, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, store: store, authStore: authStore, logger: logger}
}

// WithStorage attaches a configured R2 client. Returns the Handler for chaining.
func (h *Handler) WithStorage(r2 *storage.R2) *Handler {
	h.r2 = r2
	return h
}

// WithAI attaches a configured AI client + usage store.
func (h *Handler) WithAI(c *ai.Client, u *ai.UsageStore) *Handler {
	h.ai = c
	h.aiUsage = u
	return h
}

// WithNotifier attaches a Notifier so handlers that emit events (community
// comments etc.) can call h.notifier.Push without nil-checking. Cron jobs
// don't go through Handler — they hold their own reference to the notifier
// directly. See ADR-001 D8.
func (h *Handler) WithNotifier(n Notifier) *Handler {
	h.notifier = n
	return h
}

// WithBilling attaches the billing store so AI handlers can fall back to
// the paid credits balance after the free monthly token quota is exhausted.
// When unset, AI handlers stay free-tier-only and respond 429 once over
// the token cap (same as before billing landed).
func (h *Handler) WithBilling(bs *billing.Store) *Handler {
	h.billingStore = bs
	return h
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

// readJSON decodes the request body into dst, capping the body at 1 MiB.
// Returns (false, error-already-written) if it failed; (true, nil) on success.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "could not read body")
		return false
	}
	defer r.Body.Close()
	if len(body) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "empty body")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_json", "invalid json body")
		return false
	}
	return true
}

// readJSONOptional decodes if a body is present, otherwise leaves dst as-is.
// Use this for endpoints whose body is purely advisory.
func readJSONOptional(r *http.Request, dst any) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		return
	}
	defer r.Body.Close()
	_ = json.Unmarshal(body, dst)
}

// pathUUID parses {id} from r.PathValue("id"). On failure, writes a 400 and
// returns ok=false.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// writeDBError maps common DB errors to HTTP responses. Internal-server
// branch sends an opaque message to the client and logs full details
// server-side — the previous behaviour of echoing err.Error() leaked
// table names, column names, and constraint text, which is both a
// security smell and unhelpful for users.
func writeDBError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	slog.Error("db error", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}
