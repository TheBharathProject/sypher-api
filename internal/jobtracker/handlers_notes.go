package jobtracker

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

func (h *Handler) ListNotes(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	q := r.URL.Query()
	var catID *uuid.UUID
	if c := q.Get("categoryId"); c != "" {
		parsed, err := uuid.Parse(c)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid categoryId")
			return
		}
		catID = &parsed
	}
	notes, err := h.store.ListNotes(r.Context(), uid, catID, q.Get("search"))
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, notes)
}

// GetNote returns a single note's full content (the list endpoint only
// returns excerpt to keep payloads slim).
func (h *Handler) GetNote(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	n, err := h.store.GetNote(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (h *Handler) CreateNote(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in NoteInput
	if !readJSON(w, r, &in) {
		return
	}
	n, err := h.store.CreateNote(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, n)
}

func (h *Handler) UpdateNote(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in NoteInput
	if !readJSON(w, r, &in) {
		return
	}
	n, err := h.store.UpdateNote(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (h *Handler) DeleteNote(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteNote(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) ListCategories(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	cats, err := h.store.ListCategories(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cats)
}

func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in CategoryInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "name is required")
		return
	}
	c, err := h.store.CreateCategory(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

func (h *Handler) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteCategory(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
