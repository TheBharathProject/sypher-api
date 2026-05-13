package jobtracker

import (
	"net/http"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// GET /job-tracker/recruiters?search=...
func (h *Handler) ListRecruiters(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	search := r.URL.Query().Get("search")
	list, err := h.store.ListRecruiters(r.Context(), uid, search)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// POST /job-tracker/recruiters
func (h *Handler) CreateRecruiter(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var in RecruiterInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := validateRecruiterInput(in.Name, in.Email, in.LinkedinURL); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	rec, err := h.store.CreateRecruiter(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec)
}

// GET /job-tracker/recruiters/{id}
func (h *Handler) GetRecruiter(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	rec, err := h.store.GetRecruiter(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

// PATCH /job-tracker/recruiters/{id}
func (h *Handler) PatchRecruiter(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in RecruiterEdit
	if !readJSON(w, r, &in) {
		return
	}
	if in.Email != nil {
		if _, err := parseEmail(*in.Email); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid email")
			return
		}
	}
	if in.LinkedinURL != nil {
		if err := ValidateURL(*in.LinkedinURL); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
			return
		}
	}
	rec, err := h.store.PatchRecruiter(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

// GET /job-tracker/recruiters/{id}/phone
// Rate-limited: 5 per 5 minutes and 20 per day per user.
func (h *Handler) GetRecruiterPhone(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}

	if allowed, which := h.phoneRL.allow(uid.String()); !allowed {
		if which == "day" {
			w.Header().Set("Retry-After", "86400")
			httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited",
				"Daily limit reached: you can reveal 20 phone numbers per day. This protects sensitive contact data — try again tomorrow.")
		} else {
			w.Header().Set("Retry-After", "300")
			httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited",
				"Too many phone lookups: limit is 5 per 5 minutes. Sensitive data cannot be fetched in bulk — please wait before trying again.")
		}
		return
	}

	phone, err := h.store.GetRecruiterPhone(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RecruiterPhone{Phone: phone})
}

// DELETE /job-tracker/recruiters/{id}
func (h *Handler) DeleteRecruiter(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteRecruiter(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
