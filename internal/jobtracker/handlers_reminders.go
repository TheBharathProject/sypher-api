package jobtracker

import (
	"net/http"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// POST /job-tracker/applications/{id}/reminders
func (h *Handler) CreateReminder(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	appID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ReminderInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.TriggersAt == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "triggersAt is required")
		return
	}
	rem, err := h.store.CreateReminder(r.Context(), uid, appID, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rem)
}

// GET /job-tracker/applications/{id}/reminders
func (h *Handler) ListAppReminders(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	appID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	list, err := h.store.ListReminders(r.Context(), uid, &appID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// GET /job-tracker/reminders
func (h *Handler) ListReminders(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	list, err := h.store.ListReminders(r.Context(), uid, nil)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// PATCH /job-tracker/reminders/{id}
func (h *Handler) PatchReminder(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ReminderEdit
	if !readJSON(w, r, &in) {
		return
	}
	rem, err := h.store.PatchReminder(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rem)
}

// DELETE /job-tracker/reminders/{id}
func (h *Handler) DeleteReminder(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteReminder(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
