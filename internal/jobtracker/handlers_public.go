package jobtracker

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// PublicProfile is unauthenticated. Returns the profile if the slug is set
// and the user has set is_public = true; otherwise 404.
func (h *Handler) PublicProfile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "missing slug")
		return
	}
	pp, err := h.store.PublicProfile(r.Context(), slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "profile not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pp)
}

// PublicAnalytics is unauthenticated. Returns the user's dashboard summary +
// funnel + weekly activity if isPublic=true. Mirrors live's
// /api/public/analytics/{slug}.
func (h *Handler) PublicAnalytics(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "missing slug")
		return
	}
	pa, err := h.store.PublicAnalyticsBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "profile not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pa)
}
