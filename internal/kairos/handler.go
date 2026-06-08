package kairos

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider/kite"
)

// Handler bundles the dependencies every /kairos/* endpoint needs.
//
// pool is the only thing the handler reaches outside its own package
// (other than provider methods on the interface) — for partition
// inspection and direct chain queries.
type Handler struct {
	cfg      *config.Config
	store    *Store
	provider provider.BrokerProvider
	worker   *Pool
	logger   *slog.Logger
}

func NewHandler(cfg *config.Config, store *Store, prov provider.BrokerProvider, worker *Pool, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, store: store, provider: prov, worker: worker, logger: logger}
}

// ─────────────────────────────────────────────────────────────────────────
// /kairos/options/*
// ─────────────────────────────────────────────────────────────────────────

// GetUnderlyings returns the static list of indices the app supports
// plus the live spot for each (best-effort — failures yield zero).
func (h *Handler) GetUnderlyings(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, 3)
	for _, u := range provider.AllUnderlyings() {
		entry := map[string]any{
			"name":        string(u),
			"lot_size":    lotSize(u),
			"strike_step": strikeStep(u),
		}
		if spot, err := h.provider.FetchSpot(r.Context(), u); err == nil {
			entry["spot"] = spot.Price
			entry["change"] = spot.Change
			entry["change_pct"] = spot.ChangePct
		}
		out = append(out, entry)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"underlyings": out})
}

// GetExpiries proxies provider.FetchExpiries.
func (h *Handler) GetExpiries(w http.ResponseWriter, r *http.Request) {
	u := provider.Underlying(strings.ToUpper(r.URL.Query().Get("underlying")))
	if u == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "underlying is required")
		return
	}
	exps, err := h.provider.FetchExpiries(r.Context(), u)
	if err != nil {
		mapProviderError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(exps))
	for _, e := range exps {
		out = append(out, map[string]any{
			"date":  e.Date.Format("2006-01-02"),
			"label": formatExpiryLabel(e),
			"type":  e.Type,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"expiries": out})
}

// GetChain returns the most recent stored chain snapshot for an
// (underlying, expiry). ADR-0013 D2 — the DB is the cache; we don't
// hit the provider on this path.
func (h *Handler) GetChain(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u := provider.Underlying(strings.ToUpper(q.Get("underlying")))
	expStr := q.Get("expiry")
	if u == "" || expStr == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "underlying and expiry are required")
		return
	}
	expiry, err := time.Parse("2006-01-02", expStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "expiry must be YYYY-MM-DD")
		return
	}
	rows, snapTime, err := h.store.LatestChain(r.Context(), u, expiry)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	// Always serialise rows as [] (not null) — the FE expects an array
	// to index into. nil slices marshal to "null" by default which
	// breaks .length checks.
	if rows == nil {
		rows = []provider.ChainRow{}
	}
	resp := ChainResponse{
		Underlying:   u,
		Expiry:       expStr,
		SnapshotTime: snapTime,
		Rows:         rows,
	}
	if len(rows) > 0 {
		resp.Spot = rows[0].Spot
		resp.StalenessSeconds = int64(time.Since(snapTime).Seconds())
		// Honour If-Modified-Since (ADR-0013 D5).
		if !snapTime.IsZero() {
			w.Header().Set("Last-Modified", snapTime.UTC().Format(http.TimeFormat))
			if ims := r.Header.Get("If-Modified-Since"); ims != "" {
				if t, err := http.ParseTime(ims); err == nil && !snapTime.After(t) {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ─────────────────────────────────────────────────────────────────────────
// /kairos/strategies/*
// ─────────────────────────────────────────────────────────────────────────

// SaveStrategy persists a new strategy.
func (h *Handler) SaveStrategy(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in Strategy
	if !readJSON(w, r, &in) {
		return
	}
	if err := ValidateStrategy(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	out, err := h.store.SaveStrategy(r.Context(), uid, &in)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// ListStrategies returns the user's saved strategies.
func (h *Handler) ListStrategies(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListStrategies(r.Context(), uid)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if out == nil {
		out = []Strategy{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"strategies": out})
}

// DeleteStrategy removes a strategy by id.
func (h *Handler) DeleteStrategy(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	if err := h.store.DeleteStrategy(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "strategy not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────────────────────
// /kairos/backtest/*
// ─────────────────────────────────────────────────────────────────────────

// SubmitBacktest validates the request, inserts a pending row, enqueues
// it on the worker channel, and returns 202 with the id. ADR-0010 D2.
func (h *Handler) SubmitBacktest(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in BacktestRequest
	if !readJSON(w, r, &in) {
		return
	}
	if err := validateBacktest(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	// Rate-limit free-tier users to 5 backtests / 24h (ADR-0010 D7).
	// Premium check is left as a TODO — the auth/billing wiring is in
	// place but the kairos handler doesn't yet read is_premium. Once
	// the FE gates Premium features for Kairos, this branches on it.
	n, err := h.store.CountBacktestsLast24h(r.Context(), uid)
	if err == nil && n >= 5 {
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited",
			"free tier is limited to 5 backtests per 24 hours; upgrade for unlimited")
		return
	}
	id, err := h.store.CreateBacktest(r.Context(), uid, &in, nil)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	// Non-blocking enqueue. Channel full → next pool sweep picks it up.
	h.worker.Enqueue(id)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

// GetBacktest returns the row for polling.
func (h *Handler) GetBacktest(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	bt, err := h.store.GetBacktest(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "backtest not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bt)
}

// ListBacktests returns the user's backtest history.
func (h *Handler) ListBacktests(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	out, err := h.store.ListBacktests(r.Context(), uid, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if out == nil {
		out = []Backtest{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"backtests": out})
}

// ─────────────────────────────────────────────────────────────────────────
// /kairos/admin/*
// ─────────────────────────────────────────────────────────────────────────

// AdminSnapshot triggers a one-off chain snapshot for all underlyings,
// bypassing the cron's market-hours gate. Useful for:
//   - Seeding data outside market hours (Kite's /quote still returns the
//     last-traded values from the previous session, so we get a row.)
//   - Smoke-testing the ingest pipeline end-to-end on first deploy.
//   - Manually re-pulling after a token refresh that happened too late
//     in the market window.
//
// Auth-gated; any authenticated Kairos user can call it. There's no
// admin role concept yet — when there is, this moves behind that.
func (h *Handler) AdminSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := h.provider.IsReady(r.Context()); err != nil {
		mapProviderError(w, err)
		return
	}
	results := SnapshotChains(r.Context(), h.store, h.provider, h.logger)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"provider": h.provider.Name(),
		"results":  results,
	})
}

// ─────────────────────────────────────────────────────────────────────────
// /kairos/provider/*
// ─────────────────────────────────────────────────────────────────────────

// ProviderStatus returns whether the active provider can serve fetches.
// The FE uses this to render a banner when Kite's daily token is missing.
func (h *Handler) ProviderStatus(w http.ResponseWriter, r *http.Request) {
	out := ProviderStatus{Active: h.provider.Name()}
	if err := h.provider.IsReady(r.Context()); err != nil {
		out.Reason = err.Error()
	} else {
		out.Ready = true
	}
	// For Kite, expose a login URL so the /brokers page can render a
	// "Refresh Kite" button. Other providers will add their own URL
	// builders when implemented; type assertion keeps the contract
	// generic.
	if lurl, ok := h.provider.(interface{ LoginURL() string }); ok {
		out.LoginURL = lurl.LoginURL()
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// KiteLoginStart redirects to Kite's OAuth consent page. Public — the
// callback completes the flow.
func (h *Handler) KiteLoginStart(w http.ResponseWriter, r *http.Request) {
	prov, ok := h.provider.(interface{ LoginURL() string })
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "wrong_provider",
			"the active provider is not 'kite'")
		return
	}
	http.Redirect(w, r, prov.LoginURL(), http.StatusFound)
}

// KiteCallback exchanges Kite's `request_token` query param for an
// access_token and persists it. Kite redirects to this URL with the
// token after the user finishes OAuth.
//
// Note: this endpoint is intentionally public (Kite doesn't sign its
// redirect). Anyone can hit it, but exchanging an arbitrary string
// fails at Kite's /session/token — the most an attacker can do is
// burn a few of our rate-limited calls.
func (h *Handler) KiteCallback(w http.ResponseWriter, r *http.Request) {
	kp, ok := h.provider.(*kite.Provider)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "wrong_provider",
			"the active provider is not 'kite'")
		return
	}
	reqTok := r.URL.Query().Get("request_token")
	status := r.URL.Query().Get("status")
	if status != "" && status != "success" {
		httpx.WriteError(w, http.StatusBadRequest, "oauth_failed",
			"kite reported status="+status)
		return
	}
	if reqTok == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "request_token missing")
		return
	}
	if err := kp.ExchangeRequestToken(r.Context(), reqTok); err != nil {
		h.logger.Error("kite exchange failed", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "exchange_failed", err.Error())
		return
	}
	http.Redirect(w, r, kairosBrokersURL(h.cfg.KairosFrontendLoginRedirect), http.StatusFound)
}

// kairosBrokersURL derives the absolute URL of the FE /brokers page from
// the configured KAIROS_FRONTEND_LOGIN_REDIRECT_URL.
//
// The convention is that KAIROS_FRONTEND_LOGIN_REDIRECT_URL ends with
// "/auth/callback" (mirrors FRONTEND_LOGIN_REDIRECT_URL for pegasus).
// We trim trailing slashes first, then strip "/auth/callback", then
// append "/brokers". All of these inputs work:
//
//	"https://sypher.in/kairos/auth/callback"  → "https://sypher.in/kairos/brokers"
//	"https://sypher.in/kairos/auth/callback/" → "https://sypher.in/kairos/brokers"
//	"https://sypher.in/kairos"                → "https://sypher.in/kairos/brokers"
//	"https://sypher.in/kairos/"               → "https://sypher.in/kairos/brokers"
//
// If the env var is blank, fall back to a relative path. The browser
// follows it back to whichever origin served the redirect — usable for
// quick local poking, broken across origins. The deploy warns operators
// when this is unset.
func kairosBrokersURL(loginRedirect string) string {
	if loginRedirect == "" {
		return "/kairos/brokers"
	}
	base := strings.TrimRight(loginRedirect, "/")
	base = strings.TrimSuffix(base, "/auth/callback")
	base = strings.TrimRight(base, "/")
	return base + "/brokers"
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

func mapProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrAuthExpired):
		httpx.WriteError(w, http.StatusServiceUnavailable, "provider_auth", err.Error())
	case errors.Is(err, provider.ErrNotConfigured):
		httpx.WriteError(w, http.StatusServiceUnavailable, "provider_not_configured", err.Error())
	case errors.Is(err, provider.ErrNotImplemented):
		httpx.WriteError(w, http.StatusServiceUnavailable, "provider_not_implemented", err.Error())
	case errors.Is(err, provider.ErrRateLimited):
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	default:
		httpx.WriteError(w, http.StatusBadGateway, "provider_error", err.Error())
	}
}

func validateBacktest(in *BacktestRequest) error {
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		in.Name = "Backtest"
	}
	if !validUnderlyings[in.Underlying] {
		return errors.New("underlying must be NIFTY|BANKNIFTY|SENSEX")
	}
	if err := ValidateLegs(in.Legs); err != nil {
		return err
	}
	if !isYYYYMMDD(in.FromDate) || !isYYYYMMDD(in.ToDate) {
		return errors.New("fromDate / toDate must be YYYY-MM-DD")
	}
	if in.FromDate > in.ToDate {
		return errors.New("fromDate must be ≤ toDate")
	}
	if !isHHMM(in.EntryTime) || !isHHMM(in.ExitTime) {
		return errors.New("entryTime / exitTime must be HH:MM")
	}
	return nil
}

func isYYYYMMDD(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// readJSON decodes the request body into dst, capping at 1 MiB.
// Same pattern as internal/jobtracker/handler.go's readJSON.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "empty body")
		} else {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		}
		return false
	}
	return true
}

func lotSize(u provider.Underlying) int {
	switch u {
	case provider.UnderlyingNIFTY:
		return 75
	case provider.UnderlyingBANKNIFTY:
		return 35
	case provider.UnderlyingSENSEX:
		return 20
	}
	return 0
}

func strikeStep(u provider.Underlying) int {
	switch u {
	case provider.UnderlyingNIFTY:
		return 50
	case provider.UnderlyingBANKNIFTY:
		return 100
	case provider.UnderlyingSENSEX:
		return 200
	}
	return 0
}

func formatExpiryLabel(e provider.Expiry) string {
	label := e.Date.Format("02 Jan 2006")
	if e.Type == "weekly" {
		label += " (Weekly)"
	} else if e.Type == "monthly" {
		label += " (Monthly)"
	}
	return label
}
