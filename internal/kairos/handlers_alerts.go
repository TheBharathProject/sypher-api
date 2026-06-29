package kairos

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/alerts/*
//
// Response shapes are dictated by kairos/lib/kairos-api.ts:
//   GET    /kairos/alerts       → {alerts:[ApiAlert]}
//   POST   /kairos/alerts       → ApiAlert (bare, 201)
//   PATCH  /kairos/alerts/{id}  → ApiAlert (bare, updated row)
//   DELETE /kairos/alerts/{id}  → 204
//
// Routes are registered by the wiring task; handlers read {id} via
// r.PathValue.
// ─────────────────────────────────────────────────────────────────────────

// maxAlertThresholdAbs guards the NUMERIC(12,2) column: 10 digits before
// the point. Anything larger would error at insert; reject it as input
// instead.
const maxAlertThresholdAbs = 1e10

// ListAlerts returns all of the user's alerts, newest first.
func (h *Handler) ListAlerts(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListAlerts(r.Context(), uid)
	if err != nil {
		h.logger.Error("list alerts failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

// CreateAlert makes a new active alert. Symbol is trimmed and uppercased
// (1-32 chars), rule must be one of the four ApiAlertRule values, the
// threshold must be a finite number that fits NUMERIC(12,2). Users are
// capped at 50 active alerts (400 limit_reached).
func (h *Handler) CreateAlert(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in struct {
		Symbol    string  `json:"symbol"`
		Rule      string  `json:"rule"`
		Threshold float64 `json:"threshold"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))
	if symbol == "" || utf8.RuneCountInString(symbol) > 32 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "symbol must be 1-32 characters")
		return
	}
	if !validAlertRules[in.Rule] {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input",
			"rule must be price_above|price_below|pct_change_above|pct_change_below")
		return
	}
	if math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0) || math.Abs(in.Threshold) >= maxAlertThresholdAbs {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "threshold must be a finite number")
		return
	}
	out, err := h.store.CreateAlert(r.Context(), uid, symbol, in.Rule, in.Threshold)
	if err != nil {
		if errors.Is(err, errAlertLimit) {
			httpx.WriteError(w, http.StatusBadRequest, "limit_reached", "you can have at most 50 active alerts")
			return
		}
		h.logger.Error("create alert failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// UpdateAlert patches the alert's status (pause / re-arm / dismiss-to-
// triggered). Re-activating clears triggered_at in the store so the rule
// re-arms cleanly.
func (h *Handler) UpdateAlert(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !validAlertStatuses[in.Status] {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "status must be active|triggered|paused")
		return
	}
	out, err := h.store.UpdateAlertStatus(r.Context(), uid, id, in.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "alert not found")
			return
		}
		h.logger.Error("update alert failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// DeleteAlert removes an alert.
func (h *Handler) DeleteAlert(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	if err := h.store.DeleteAlert(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "alert not found")
			return
		}
		h.logger.Error("delete alert failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
