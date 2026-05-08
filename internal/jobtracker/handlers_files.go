package jobtracker

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// requireR2 asserts the R2 client is initialised; otherwise writes a 503.
func (h *Handler) requireR2(w http.ResponseWriter) bool {
	if h.r2 == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "storage_unavailable",
			"file storage is not configured on this deployment")
		return false
	}
	return true
}

func storageKey(userID uuid.UUID, kind, originalName string) string {
	id := uuid.New()
	safe := strings.ReplaceAll(strings.ReplaceAll(originalName, "/", "_"), "\\", "_")
	if len(safe) > 100 {
		safe = safe[len(safe)-100:]
	}
	return fmt.Sprintf("job-tracker/%s/%s/%s-%s", userID, kind, id, safe)
}

// ----------------------------------------------------------------------------
// Resumes
// ----------------------------------------------------------------------------

// fileSlotLimit caps slot-bearing files per kind. Non-slot uploads
// (slot=NULL, e.g. AI source files) bypass the cap.
const fileSlotLimit = 5

// resumesEnvelope is the response shape for /resumes — wraps the list with
// count + limit so the frontend can display "X / 5" without computing.
type filesEnvelope struct {
	Resumes      []File `json:"resumes,omitempty"`
	CoverLetters []File `json:"coverLetters,omitempty"`
	Count        int    `json:"count"`
	Limit        int    `json:"limit"`
}

func countSlotted(files []File) int {
	n := 0
	for _, f := range files {
		if f.Slot != nil {
			n++
		}
	}
	return n
}

func (h *Handler) ListResumes(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	files, err := h.store.ListFiles(r.Context(), uid, "resume")
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, filesEnvelope{
		Resumes: files,
		Count:   countSlotted(files),
		Limit:   fileSlotLimit,
	})
}

func (h *Handler) RequestResumeUploadURL(w http.ResponseWriter, r *http.Request) {
	if !h.requireR2(w) {
		return
	}
	h.requestUploadURL(w, r, "resume")
}

func (h *Handler) FinalizeResume(w http.ResponseWriter, r *http.Request) {
	h.finalizeFile(w, r)
}

func (h *Handler) DeleteResume(w http.ResponseWriter, r *http.Request) {
	h.deleteFile(w, r)
}

// ResumeViewURL returns a short-lived presigned GET URL the browser can use
// as an iframe src to render the file inline. Tenant-isolated via GetFile —
// returns 404 if the file isn't owned by the caller. Same pattern works for
// cover letters via the unified route below.
func (h *Handler) ResumeViewURL(w http.ResponseWriter, r *http.Request) {
	h.fileViewURL(w, r, "resume")
}

func (h *Handler) CoverLetterViewURL(w http.ResponseWriter, r *http.Request) {
	h.fileViewURL(w, r, "cover_letter")
}

func (h *Handler) fileViewURL(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.requireR2(w) {
		return
	}
	uid := auth.MustUserID(r.Context())
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid file id")
		return
	}
	file, err := h.store.GetFile(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "file not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if file.Kind != kind {
		httpx.WriteError(w, http.StatusBadRequest, "wrong_kind", "file kind mismatch")
		return
	}
	url, err := h.r2.PresignGet(r.Context(), file.StorageKey, 5*time.Minute)
	if err != nil {
		h.logger.Error("presign get", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "presign_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"viewUrl":   url,
		"expiresIn": int((5 * time.Minute).Seconds()),
	})
}

// ResumeUsage returns how often the resume has been used. Today that's the
// AI-report count; once applications gain a resume_id picker the response
// will grow to {aiReports, applications}. Wraps the backend value the
// frontend reads to decide whether to render the "Reviewed N times" badge.
func (h *Handler) ResumeUsage(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid resume id")
		return
	}
	count, err := h.store.ResumeUsage(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "resume not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"aiReports": count,
	})
}

// ----------------------------------------------------------------------------
// Cover letters
// ----------------------------------------------------------------------------

func (h *Handler) ListCoverLetters(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	files, err := h.store.ListFiles(r.Context(), uid, "cover_letter")
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, filesEnvelope{
		CoverLetters: files,
		Count:        countSlotted(files),
		Limit:        fileSlotLimit,
	})
}

func (h *Handler) RequestCoverLetterUploadURL(w http.ResponseWriter, r *http.Request) {
	if !h.requireR2(w) {
		return
	}
	h.requestUploadURL(w, r, "cover_letter")
}

func (h *Handler) FinalizeCoverLetter(w http.ResponseWriter, r *http.Request) {
	h.finalizeFile(w, r)
}

func (h *Handler) DeleteCoverLetter(w http.ResponseWriter, r *http.Request) {
	h.deleteFile(w, r)
}

// ----------------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------------

func (h *Handler) requestUploadURL(w http.ResponseWriter, r *http.Request, kind string) {
	uid := auth.MustUserID(r.Context())
	var in FileInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.FileName == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fileName is required")
		return
	}
	if in.FileSize > 25<<20 { // 25 MiB cap
		httpx.WriteError(w, http.StatusBadRequest, "too_large", "file too large (max 25 MiB)")
		return
	}
	if in.Slot != nil && (*in.Slot < 1 || *in.Slot > 5) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "slot must be between 1 and 5")
		return
	}
	in.Kind = kind

	key := storageKey(uid, kind, in.FileName)
	file, orphanKeys, err := h.store.CreateFile(r.Context(), uid, kind, in.Label, in.FileName, in.MimeType, key, in.FileSize, in.Slot)
	if err != nil {
		writeDBError(w, err)
		return
	}
	// Clean up the R2 objects we just unparented (replace-in-slot semantics).
	if h.r2 != nil {
		for _, k := range orphanKeys {
			if err := h.r2.Delete(r.Context(), k); err != nil {
				h.logger.Warn("r2 cleanup failed", "key", k, "err", err)
			}
		}
	}

	contentType := in.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	uploadURL, err := h.r2.PresignPut(r.Context(), key, contentType, 10*time.Minute)
	if err != nil {
		h.logger.Error("presign put", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "presign_failed", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploadUrl": uploadURL,
		"file":      file,
	})
}

// UpdateFileLabel handles PATCH /resumes/{id} and /cover-letters/{id}.
// Body: {label: string}. Empty string clears the label.
func (h *Handler) UpdateFileLabel(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Label string `json:"label"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.store.UpdateFileLabel(r.Context(), uid, id, in.Label); err != nil {
		writeDBError(w, err)
		return
	}
	file, err := h.store.GetFile(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, file)
}

func (h *Handler) finalizeFile(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		FileSize int64 `json:"fileSize"`
	}
	readJSONOptional(r, &in)
	if err := h.store.FinalizeFile(r.Context(), uid, id, in.FileSize); err != nil {
		writeDBError(w, err)
		return
	}
	file, err := h.store.GetFile(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, file)
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	key, err := h.store.DeleteFile(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "file not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if h.r2 != nil {
		if err := h.r2.Delete(r.Context(), key); err != nil {
			h.logger.Warn("r2 delete failed", "key", key, "err", err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
