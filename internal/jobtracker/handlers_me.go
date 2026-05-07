package jobtracker

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// GetMe returns the current authenticated user.
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	u, err := h.authStore.GetUserByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

type updateNameInput struct {
	Name string `json:"name"`
}

// UpdateName sets the user's display name.
func (h *Handler) UpdateName(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in updateNameInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" || len(in.Name) > 120 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "name must be 1-120 chars")
		return
	}
	if err := h.authStore.UpdateUserName(r.Context(), uid, in.Name); err != nil {
		writeDBError(w, err)
		return
	}
	h.GetMe(w, r)
}

type updateTimezoneInput struct {
	Timezone string `json:"timezone"`
}

// UpdateTimezone sets the user's IANA timezone.
func (h *Handler) UpdateTimezone(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in updateTimezoneInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Timezone == "" || len(in.Timezone) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "timezone must be 1-64 chars")
		return
	}
	if err := h.authStore.UpdateUserTimezone(r.Context(), uid, in.Timezone); err != nil {
		writeDBError(w, err)
		return
	}
	h.GetMe(w, r)
}

type issueTokenInput struct {
	Label string `json:"label"`
}

// IssueAPIToken creates a new long-lived per-user token for the browser
// extension. The plain token is returned exactly once.
func (h *Handler) IssueAPIToken(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in issueTokenInput
	readJSONOptional(r, &in)
	plain, meta, err := h.authStore.IssueAPIToken(r.Context(), uid, in.Label)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"token": plain,
		"id":    meta.ID,
		"prefix": meta.Prefix,
		"label":  meta.Label,
	})
}
