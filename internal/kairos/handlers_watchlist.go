package kairos

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/watchlists/*
//
// Response shapes are dictated by kairos/lib/kairos-api.ts:
//   GET    /kairos/watchlists                      → {watchlists:[ApiWatchlist]}
//   POST   /kairos/watchlists                      → ApiWatchlist (bare)
//   DELETE /kairos/watchlists/{id}                 → 204
//   POST   /kairos/watchlists/{id}/items           → ApiWatchlistItem (bare)
//   DELETE /kairos/watchlists/{id}/items/{itemId}  → 204
//
// Routes are registered by the wiring task; handlers read {id} and
// {itemId} via r.PathValue.
// ─────────────────────────────────────────────────────────────────────────

// ListWatchlists ensures the default list exists, then returns all of
// the user's lists with items (spec §3.4 — auto-create on first GET).
func (h *Handler) ListWatchlists(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListWatchlists(r.Context(), uid)
	if err != nil {
		h.logger.Error("list watchlists failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"watchlists": out})
}

// CreateWatchlist makes a new empty list. Name is trimmed and must be
// 1-60 characters.
func (h *Handler) CreateWatchlist(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 60 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "name must be 1-60 characters")
		return
	}
	out, err := h.store.CreateWatchlist(r.Context(), uid, name)
	if err != nil {
		h.logger.Error("create watchlist failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// DeleteWatchlist removes a list (and, via cascade, its items).
func (h *Handler) DeleteWatchlist(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	if err := h.store.DeleteWatchlist(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "watchlist not found")
			return
		}
		h.logger.Error("delete watchlist failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddWatchlistItem appends a symbol to a list. Symbol is trimmed and
// uppercased, 1-32 chars; exchange defaults to NSE. Lists are capped
// at 200 items (400 limit_reached); re-adding a symbol is a 409.
func (h *Handler) AddWatchlistItem(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	listID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	var in struct {
		Symbol   string `json:"symbol"`
		Exchange string `json:"exchange"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))
	if symbol == "" || utf8.RuneCountInString(symbol) > 32 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "symbol must be 1-32 characters")
		return
	}
	exchange := strings.ToUpper(strings.TrimSpace(in.Exchange))
	if exchange == "" {
		exchange = "NSE"
	}
	out, err := h.store.AddItem(r.Context(), uid, listID, symbol, exchange)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			httpx.WriteError(w, http.StatusNotFound, "not_found", "watchlist not found")
		case errors.Is(err, errWatchlistItemLimit):
			httpx.WriteError(w, http.StatusBadRequest, "limit_reached", "watchlist is limited to 200 symbols")
		case errors.Is(err, errWatchlistDuplicate):
			httpx.WriteError(w, http.StatusConflict, "duplicate_item", "symbol is already on this watchlist")
		default:
			h.logger.Error("add watchlist item failed", "err", err, "request_id", httpx.RequestID(r.Context()))
			httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// RemoveWatchlistItem deletes one symbol from a list.
func (h *Handler) RemoveWatchlistItem(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	listID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	itemID, err := uuid.Parse(r.PathValue("itemId"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid item id")
		return
	}
	if err := h.store.RemoveItem(r.Context(), uid, listID, itemID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "watchlist item not found")
			return
		}
		h.logger.Error("remove watchlist item failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
