package jobtracker

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ============================================================================
// Resume Builder — HTTP handlers.
//
// Drafts CRUD + LaTeX rendering + save-to-Vault flow. Compilation lives in
// resume_builder_render.go; the storage path reuses CreateFile / R2.Put so
// the saved PDF lives in the existing job_tracker.files surface and shows
// up in /resumes (the Vault) like any other resume.
// ============================================================================

// maxResumeBuilderTitle bounds the title field. 200 chars is generous —
// most are <30 — but the cap prevents pathological localStorage drafts
// from polluting the listing rail.
const maxResumeBuilderTitle = 200

// ListResumeBuilderDrafts — GET /job-tracker/resume-builder/drafts
//
// Returns the user's drafts as summary rows (no content payload). Newest
// first by updated_at.
func (h *Handler) ListResumeBuilderDrafts(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	items, err := h.store.ListResumeBuilderDrafts(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateResumeBuilderDraft — POST /job-tracker/resume-builder/drafts
func (h *Handler) CreateResumeBuilderDraft(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in ResumeBuilderDraftInput
	if !readJSON(w, r, &in) {
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		in.Title = "Untitled resume"
	}
	if len(in.Title) > maxResumeBuilderTitle {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input",
			fmt.Sprintf("title must be at most %d chars", maxResumeBuilderTitle))
		return
	}
	draft, err := h.store.CreateResumeBuilderDraft(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, draft)
}

// GetResumeBuilderDraft — GET /job-tracker/resume-builder/drafts/{id}
func (h *Handler) GetResumeBuilderDraft(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	draft, err := h.store.GetResumeBuilderDraft(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, draft)
}

// PatchResumeBuilderDraft — PATCH /job-tracker/resume-builder/drafts/{id}
//
// FE autosaves through this endpoint every 2s of inactivity. Body is the
// partial ResumeBuilderDraftEdit; missing fields are left unchanged.
func (h *Handler) PatchResumeBuilderDraft(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ResumeBuilderDraftEdit
	if !readJSON(w, r, &in) {
		return
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if len(t) > maxResumeBuilderTitle {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input",
				fmt.Sprintf("title must be at most %d chars", maxResumeBuilderTitle))
			return
		}
		in.Title = &t
	}
	draft, err := h.store.PatchResumeBuilderDraft(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, draft)
}

// DeleteResumeBuilderDraft — DELETE /job-tracker/resume-builder/drafts/{id}
func (h *Handler) DeleteResumeBuilderDraft(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteResumeBuilderDraft(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RenderResumeBuilderTex — POST /job-tracker/resume-builder/drafts/{id}/render/tex
//
// Returns raw LaTeX source as text/plain. Useful for the "View source"
// affordance in the export modal + lets power users tweak the .tex offline.
func (h *Handler) RenderResumeBuilderTex(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	draft, err := h.store.GetResumeBuilderDraft(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	tex, err := RenderResumeBuilderTeX(draft.Content, draft.TemplateID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "render_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(tex))
}

// RenderResumeBuilderPDF — POST /job-tracker/resume-builder/drafts/{id}/render/pdf
//
// Compiles LaTeX → PDF via Tectonic and streams the binary back. Returns
// 503 if Tectonic isn't installed (dev environments) so the FE can fall
// back to showing the .tex preview.
func (h *Handler) RenderResumeBuilderPDF(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	draft, err := h.store.GetResumeBuilderDraft(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	tex, err := RenderResumeBuilderTeX(draft.Content, draft.TemplateID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "render_failed", err.Error())
		return
	}
	pdfBytes, err := RenderResumeBuilderPDF(r.Context(), h.latexServiceURL, tex)
	if err != nil {
		if errors.Is(err, errLatexServiceUnavailable) {
			httpx.WriteError(w, http.StatusServiceUnavailable, "latex_service_unavailable",
				"PDF compilation service is offline or not configured on this server")
			return
		}
		// LaTeX compile failures land here — message includes the log tail
		// from the sidecar so the FE can render the offending line.
		httpx.WriteError(w, http.StatusBadRequest, "compile_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="resume.pdf"`)
	_, _ = w.Write(pdfBytes)
}

// SaveResumeBuilderToVault — POST /job-tracker/resume-builder/drafts/{id}/save-to-vault
//
// End-to-end: render PDF → upload to R2 → create job_tracker.files row
// with kind='resume' in the requested slot (replaces any existing file
// in that slot). Returns the new file metadata. The Vault UI picks it up
// on next refresh.
func (h *Handler) SaveResumeBuilderToVault(w http.ResponseWriter, r *http.Request) {
	if !h.requireR2(w) {
		return
	}
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in SaveToVaultInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Slot < 1 || in.Slot > 5 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "slot must be between 1 and 5")
		return
	}

	draft, err := h.store.GetResumeBuilderDraft(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "draft not found")
			return
		}
		writeDBError(w, err)
		return
	}

	tex, err := RenderResumeBuilderTeX(draft.Content, draft.TemplateID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "render_failed", err.Error())
		return
	}
	pdfBytes, err := RenderResumeBuilderPDF(r.Context(), h.latexServiceURL, tex)
	if err != nil {
		if errors.Is(err, errLatexServiceUnavailable) {
			httpx.WriteError(w, http.StatusServiceUnavailable, "latex_service_unavailable",
				"PDF compilation service is offline or not configured on this server")
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "compile_failed", err.Error())
		return
	}

	// File name shown in the Vault. Keep readable; strip pathological chars.
	fileName := safeVaultFilename(draft.Title)
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = draft.Title
	}
	slot := in.Slot
	key := storageKey(uid, "resume", fileName)

	// CreateFile handles replace-in-slot semantics — returns orphan keys
	// from the previous occupant so we can clean R2 after the new PUT.
	file, orphanKeys, err := h.store.CreateFile(
		r.Context(), uid,
		"resume", label, fileName, "application/pdf",
		key, int64(len(pdfBytes)), &slot,
	)
	if err != nil {
		writeDBError(w, err)
		return
	}

	if err := h.r2.Put(r.Context(), key, "application/pdf", pdfBytes); err != nil {
		h.logger.Error("r2 put after slot replace", "err", err, "key", key)
		httpx.WriteError(w, http.StatusInternalServerError, "upload_failed", "failed to upload compiled PDF")
		return
	}

	// Mark file as uploaded (sets uploaded_at + final file_size from server-
	// side bytes — more accurate than the client-supplied value would be).
	if err := h.store.FinalizeFile(r.Context(), uid, file.ID, int64(len(pdfBytes))); err != nil {
		h.logger.Error("finalize after r2 put", "err", err, "file_id", file.ID)
		// File row + R2 object are both present; finalization just sets
		// uploaded_at. Surface as 500 but the user can usually refresh.
		writeDBError(w, err)
		return
	}

	// Clean up the R2 objects replaced by this slot write. Best-effort —
	// failures don't fail the request (the user has their new file).
	for _, k := range orphanKeys {
		if err := h.r2.Delete(r.Context(), k); err != nil {
			h.logger.Warn("r2 cleanup after vault save", "key", k, "err", err)
		}
	}

	// Re-fetch so the response reflects uploaded_at being set.
	fresh, err := h.store.GetFile(r.Context(), uid, file.ID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, fresh)
}

// safeVaultFilename produces a filesystem-safe filename from the draft
// title — replaces whitespace + path separators with hyphens, keeps
// alphanumerics, and ensures the result ends in ".pdf".
func safeVaultFilename(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "resume"
	}
	var b strings.Builder
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "resume"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out + ".pdf"
}
