package kairos

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/greeks"
	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
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
	// authStore is optional (jobtracker With* idiom). It powers the
	// premium check in SubmitBacktest (nil → everyone gets free-tier
	// limits, fail-closed) and the admin user endpoints (nil → 503).
	authStore *auth.Store
	// marketData is optional (same idiom; set via WithMarketData in
	// handlers_screener.go). It powers the screener's best-effort
	// live-price merge — nil just means pricesLive=false.
	marketData *marketdata.Service
}

func NewHandler(cfg *config.Config, store *Store, prov provider.BrokerProvider, worker *Pool, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, store: store, provider: prov, worker: worker, logger: logger}
}

// WithAuthStore attaches the auth store. Returns the Handler for chaining.
func (h *Handler) WithAuthStore(as *auth.Store) *Handler {
	h.authStore = as
	return h
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

// GetChain returns a stored chain snapshot for an (underlying, expiry),
// enriched with read-time greeks (ADR-0009 D3). ADR-0013 D2 — the DB is
// the cache; we don't hit the provider on this path.
//
// Without ts= it serves the latest snapshot. With ts= (RFC3339) it's
// the time machine: the latest snapshot at-or-before ts on ts's date.
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
	var (
		rows     []provider.ChainRow
		snapTime time.Time
		tsStr    = q.Get("ts")
	)
	if tsStr != "" {
		ts, perr := time.Parse(time.RFC3339, tsStr)
		if perr != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "ts must be RFC3339")
			return
		}
		rows, snapTime, err = h.store.ChainAt(r.Context(), u, expiry, ts)
	} else {
		rows, snapTime, err = h.store.LatestChain(r.Context(), u, expiry)
	}
	if err != nil {
		h.logger.Error("load chain failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	resp := ChainResponse{
		Underlying:   u,
		Expiry:       expStr,
		SnapshotTime: snapTime,
		// enrichChain always returns a non-nil slice — the FE expects an
		// array to index into; a nil slice marshals to "null" and breaks
		// .length checks.
		Rows: enrichChain(rows, expiry, snapTime),
	}
	if len(rows) > 0 {
		resp.Spot = rows[0].Spot
		resp.StalenessSeconds = int64(time.Since(snapTime).Seconds())
		// Honour If-Modified-Since (ADR-0013 D5) — latest-snapshot path
		// only. A time-machine response is keyed by ts, not by recency;
		// matching it against the browser's cached "latest" copy would
		// 304 the wrong payload.
		if tsStr == "" && !snapTime.IsZero() {
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

// ChainTimestamps returns every snapshot time recorded for an underlying
// on a trading day — the positions of the FE's time-machine scrubber.
// Each entry is valid as GetChain's ts= param.
func (h *Handler) ChainTimestamps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u := provider.Underlying(strings.ToUpper(q.Get("underlying")))
	dateStr := q.Get("date")
	if u == "" || dateStr == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "underlying and date are required")
		return
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "date must be YYYY-MM-DD")
		return
	}
	times, err := h.store.SnapshotTimes(r.Context(), u, date)
	if err != nil {
		h.logger.Error("load snapshot times failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.UTC().Format(time.RFC3339))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"timestamps": out})
}

// IntradayAnalytics returns the per-snapshot PCR / max-pain / spot
// series for an (underlying, expiry) on a trading day — the data behind
// the intraday analytics chart.
func (h *Handler) IntradayAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u := provider.Underlying(strings.ToUpper(q.Get("underlying")))
	expStr := q.Get("expiry")
	dateStr := q.Get("date")
	if u == "" || expStr == "" || dateStr == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "underlying, expiry and date are required")
		return
	}
	expiry, err := time.Parse("2006-01-02", expStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "expiry must be YYYY-MM-DD")
		return
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "date must be YYYY-MM-DD")
		return
	}
	snaps, err := h.store.DaySnapshots(r.Context(), u, expiry, date)
	if err != nil {
		h.logger.Error("load day snapshots failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	points := make([]IntradayPoint, 0, len(snaps))
	for _, s := range snaps {
		points = append(points, IntradayPoint{
			TS:      s.TS,
			PCR:     computePCR(s.Rows),
			MaxPain: computeMaxPain(s.Rows),
			Spot:    s.Spot,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"points": points})
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
		h.logger.Error("save strategy failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// ListStrategies returns the user's saved strategies.
func (h *Handler) ListStrategies(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListStrategies(r.Context(), uid)
	if err != nil {
		h.logger.Error("list strategies failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
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
		h.logger.Error("delete strategy failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
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
	if !h.gateBacktest(w, r, uid, &in) {
		return
	}
	id, err := h.store.CreateBacktest(r.Context(), uid, &in, nil)
	if err != nil {
		h.logger.Error("create backtest failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	// Non-blocking enqueue. Channel full → next pool sweep picks it up.
	h.worker.Enqueue(id)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

// gateBacktest enforces the server-side premium rules (spec §3.6,
// ADR-0010 D7) after validateBacktest has confirmed date formats:
//
//	everyone: toDate ≤ today, fromDate < toDate
//	free:     fromDate ≥ today−6 months AND <5 backtests in 24h
//	premium:  fromDate ≥ today−3 years, unlimited count
//
// Returns false with the response already written when the request is
// rejected. A nil/unreachable authStore fails closed to free-tier
// limits rather than 500ing the submit path.
func (h *Handler) gateBacktest(w http.ResponseWriter, r *http.Request, uid uuid.UUID, in *BacktestRequest) bool {
	// Already format-validated by validateBacktest; errors can't happen.
	fromDate, _ := time.Parse("2006-01-02", in.FromDate)
	toDate, _ := time.Parse("2006-01-02", in.ToDate)
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	if toDate.After(today) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "toDate must not be in the future")
		return false
	}
	if !fromDate.Before(toDate) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fromDate must be before toDate")
		return false
	}

	isPremium := false
	if h.authStore != nil {
		u, err := h.authStore.GetUserByID(r.Context(), uid)
		if err != nil {
			h.logger.Error("backtest premium lookup failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		} else {
			isPremium = u.IsPremium
		}
	}

	if isPremium {
		if fromDate.Before(today.AddDate(-3, 0, 0)) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input",
				"backtests are limited to the last 3 years of data")
			return false
		}
		return true
	}
	if fromDate.Before(today.AddDate(0, -6, 0)) {
		httpx.WriteError(w, http.StatusForbidden, "premium_required",
			"Free accounts can backtest the last 6 months")
		return false
	}
	n, err := h.store.CountBacktestsSince(r.Context(), uid, now.Add(-24*time.Hour))
	if err != nil {
		h.logger.Error("backtest rate-limit count failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return false
	}
	if n >= 5 {
		httpx.WriteError(w, http.StatusForbidden, "premium_required",
			"Free limit: 5 backtests per 24h")
		return false
	}
	return true
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
		h.logger.Error("load backtest failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
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
		h.logger.Error("list backtests failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
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

// enrichChain attaches read-time greeks to stored chain rows
// (ADR-0009 D3 — greeks are computed here, never persisted).
//
// tte = (expiry - snapshotTime) in days/365, floored at half a day so
// expiry-day rows don't divide by zero. Rows that can't be enriched
// keep zero greeks rather than failing the whole request: untraded
// strikes (LTP==0), rows missing a spot, and prices outside
// no-arbitrage bounds (stale deep-ITM quotes do this routinely).
//
// Rounding mirrors chain-data.ts (iv 2dp in percent, delta 3dp,
// gamma 4dp, theta/vega 2dp) so the FE renders identical numbers.
// Delta is abs() for PE because the FE shows both sides positive.
func enrichChain(rows []provider.ChainRow, expiry, snapTime time.Time) []EnrichedChainRow {
	out := make([]EnrichedChainRow, 0, len(rows))
	tte := expiry.Sub(snapTime).Hours() / 24 / 365
	if tte < 0.5/365 {
		tte = 0.5 / 365
	}
	for _, row := range rows {
		e := EnrichedChainRow{ChainRow: row}
		if row.LTP > 0 && row.Spot > 0 && row.Strike > 0 {
			isCall := row.OptionType == provider.OptionTypeCE
			iv, err := greeks.ImpliedVol(isCall, row.LTP, row.Spot, float64(row.Strike), greeks.RiskFreeRate, tte)
			if err == nil {
				g := greeks.Compute(isCall, row.Spot, float64(row.Strike), greeks.RiskFreeRate, iv, tte)
				delta := g.Delta
				if !isCall {
					delta = math.Abs(delta)
				}
				e.IV = roundTo(g.IV*100, 2)
				e.Delta = roundTo(delta, 3)
				e.Gamma = roundTo(g.Gamma, 4)
				e.Theta = roundTo(g.Theta, 2)
				e.Vega = roundTo(g.Vega, 2)
			}
		}
		out = append(out, e)
	}
	return out
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// computePCR is total PE OI over total CE OI for one snapshot. Returns
// 0 when there is no CE OI — the API contract's divide-by-zero guard
// (the FE's computePCR defaults to 1 for display, but the analytics
// series wants a value that reads as "no data", not "balanced").
func computePCR(rows []DayChainRow) float64 {
	var ceOI, peOI int64
	for _, r := range rows {
		switch r.OptionType {
		case provider.OptionTypeCE:
			ceOI += r.OI
		case provider.OptionTypePE:
			peOI += r.OI
		}
	}
	if ceOI == 0 {
		return 0
	}
	return float64(peOI) / float64(ceOI)
}

// computeMaxPain returns the expiry-settlement strike that minimises
// option writers' total intrinsic payout. Same semantics as
// computeMaxPain in kairos/app/options/chain-data.ts: for candidate K,
// CE writers pay oi*(K-strike) on strikes below K and PE writers pay
// oi*(strike-K) on strikes above K (strict inequalities); ties go to
// the lowest strike. Returns 0 for an empty chain.
func computeMaxPain(rows []DayChainRow) int {
	ceOI := make(map[int]int64)
	peOI := make(map[int]int64)
	seen := make(map[int]bool)
	for _, r := range rows {
		seen[r.Strike] = true
		switch r.OptionType {
		case provider.OptionTypeCE:
			ceOI[r.Strike] += r.OI
		case provider.OptionTypePE:
			peOI[r.Strike] += r.OI
		}
	}
	if len(seen) == 0 {
		return 0
	}
	strikes := make([]int, 0, len(seen))
	for k := range seen {
		strikes = append(strikes, k)
	}
	sort.Ints(strikes)
	best := strikes[0]
	minLoss := int64(math.MaxInt64)
	for _, k := range strikes {
		var loss int64
		for _, s := range strikes {
			if s < k {
				loss += ceOI[s] * int64(k-s)
			}
			if s > k {
				loss += peOI[s] * int64(s-k)
			}
		}
		if loss < minLoss {
			minLoss = loss
			best = k
		}
	}
	return best
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
