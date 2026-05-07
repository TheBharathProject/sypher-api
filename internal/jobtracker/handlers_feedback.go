package jobtracker

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

func (h *Handler) PostFeedback(w http.ResponseWriter, r *http.Request) {
	var in FeedbackInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Message == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "message is required")
		return
	}
	var uid *uuid.UUID
	if v, ok := auth.UserID(r.Context()); ok {
		uid = &v
	}
	if err := h.store.InsertFeedback(r.Context(), uid, in); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"received": true})
}
