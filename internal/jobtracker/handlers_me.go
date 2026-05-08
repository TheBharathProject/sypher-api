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
		"token":  plain,
		"id":     meta.ID,
		"prefix": meta.Prefix,
		"label":  meta.Label,
	})
}

type updateEmailPrefsInput struct {
	Enabled bool `json:"enabled"`
}

// UpdateEmailPrefs flips the per-user email opt-in. Free users attempting
// to enable get a 402 Payment Required — emails are a premium perk per
// docs/adr/0002-premium-email-gating.md (D2/D4).
//
// Disabling is allowed for free users too (they're effectively already
// disabled, but persisting their explicit choice prevents surprise emails
// if they upgrade later — opt-in stays where they last left it).
func (h *Handler) UpdateEmailPrefs(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in updateEmailPrefsInput
	if !readJSON(w, r, &in) {
		return
	}

	// Premium gate only fires when enabling. Free users can disable freely
	// — that just stamps a more explicit "no thanks" on their row.
	if in.Enabled {
		u, err := h.authStore.GetUserByID(r.Context(), uid)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if !u.IsPremium {
			httpx.WriteError(w, http.StatusPaymentRequired, "premium_required",
				"Email notifications are a premium feature. Upgrade in Settings.")
			return
		}
	}

	updated, err := h.authStore.SetEmailPref(r.Context(), uid, in.Enabled)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

// DeleteAccount drops the user from auth.users; ON DELETE CASCADE on every
// product table tears down everything they own (applications, notes,
// resumes/cover-letters in DB metadata only — actual R2 objects live until
// the bucket lifecycle policy reaps them, which is fine since the DB no
// longer references them).
//
// Returns 204 on success. Caller is expected to clearToken() and bounce the
// user to the apex client-side.
func (h *Handler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	if err := h.authStore.DeleteUser(r.Context(), uid); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
