package jobtracker

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/billing"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ----------------------------------------------------------------------------
// /ai/resume/tweaks — versioned AI rewrites with revision-chain history.
//
// Endpoints (all mounted via routes.go inside the requireUser group):
//
//   POST   /job-tracker/ai/resume/tweaks            create a tweak (20 credits)
//   GET    /job-tracker/ai/resume/tweaks            list summaries (?applicationId=)
//   GET    /job-tracker/ai/resume/tweaks/{id}       full row
//   PATCH  /job-tracker/ai/resume/tweaks/{id}       save title / user_edits (free)
//   DELETE /job-tracker/ai/resume/tweaks/{id}       delete one version
//
// Credit gating goes through h.gateAICredit; the source-resume text is
// resolved by Store.resolveTweakSource which honours the precedence:
// explicit sourceText > parent tweak's text (continuation) > extracted
// text from a Vault PDF.
// ----------------------------------------------------------------------------

// CreateResumeTweak handles POST /ai/resume/tweaks.
//
// Body fields (see ResumeTweakInput):
//   - title              required
//   - prompt             required (the JD or cover letter to tweak against)
//   - sourceText         optional; pasted source resume
//   - sourceFileId       optional; UUID of a Vault file (we extract text)
//   - parentId           optional; continuation from a prior tweak
//   - applicationId      optional; ties this tweak to a tracked application
//
// One of sourceText / sourceFileId / parentId must be supplied.
func (h *Handler) CreateResumeTweak(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	if !h.requireAI(w) {
		return
	}

	var in ResumeTweakInput
	if !readJSON(w, r, &in) {
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "title is required")
		return
	}
	if in.Prompt == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "prompt is required")
		return
	}

	parentID, ok := optionalUUID(w, in.ParentID, "parentId")
	if !ok {
		return
	}
	sourceFileID, ok := optionalUUID(w, in.SourceFileID, "sourceFileId")
	if !ok {
		return
	}
	applicationID, ok := optionalUUID(w, in.ApplicationID, "applicationId")
	if !ok {
		return
	}

	// Resolve the source text — pasted text wins, else continuation, else
	// extract from a Vault file. resolveTweakSource also forwards the
	// source-file ref so the new row's source_file_id stays consistent.
	sourceText, resolvedFileID, err := h.store.resolveTweakSource(
		r.Context(), uid, parentID, sourceFileID, strings.TrimSpace(in.SourceText),
	)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}

	// If we still don't have source text and a sourceFileId was given,
	// fetch+extract via the existing files path. Matches the pattern in
	// GenerateResumeReport / GenerateCoverLetter so behaviour is uniform.
	if sourceText == "" && resolvedFileID != nil {
		if !h.requireR2(w) {
			return
		}
		file, err := h.store.GetFile(r.Context(), uid, *resolvedFileID)
		if err != nil {
			writeDBError(w, err)
			return
		}
		body, err := h.r2.FetchAll(r.Context(), file.StorageKey)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "fetch_failed", err.Error())
			return
		}
		sourceText = extractText(body, file.MimeType, file.FileName)
	}
	if strings.TrimSpace(sourceText) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "empty_resume",
			"could not resolve any source resume text (sourceText, sourceFileId, or parentId required)")
		return
	}

	// Credit gate — free quota first, then 20 credits.
	if _, ok := h.gateAICredit(w, r, uid, billing.CostResumeTweak, billing.ReasonResumeTweak); !ok {
		return
	}

	res, err := h.ai.ResumeTweak(r.Context(), sourceText, in.Prompt)
	if err != nil {
		h.logger.Error("ai resume tweak", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "ai_failed", err.Error())
		return
	}
	if err := h.aiUsage.Record(r.Context(), uid, "resume_tweak", res.TokensIn, res.TokensOut); err != nil {
		h.logger.Warn("ai usage record", "err", err)
	}

	saved, err := h.store.CreateResumeTweak(r.Context(), CreateResumeTweakArgs{
		UserID:        uid,
		ParentID:      parentID,
		ApplicationID: applicationID,
		SourceFileID:  resolvedFileID,
		Title:         in.Title,
		SourceText:    sourceText,
		Prompt:        in.Prompt,
		TweakedText:   res.Text,
		TokensIn:      res.TokensIn,
		TokensOut:     res.TokensOut,
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// ListResumeTweaks handles GET /ai/resume/tweaks?applicationId=...
func (h *Handler) ListResumeTweaks(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())

	var appID uuid.UUID
	if v := r.URL.Query().Get("applicationId"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_query", "invalid applicationId")
			return
		}
		appID = id
	}

	rows, err := h.store.ListResumeTweaks(r.Context(), uid, appID, 50)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if rows == nil {
		rows = []ResumeTweakSummary{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// GetResumeTweak handles GET /ai/resume/tweaks/{id}.
func (h *Handler) GetResumeTweak(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	row, err := h.store.GetResumeTweak(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "tweak not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// PatchResumeTweak handles PATCH /ai/resume/tweaks/{id}. No credit
// charge — editing an existing version is free.
func (h *Handler) PatchResumeTweak(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ResumeTweakEdit
	if !readJSON(w, r, &in) {
		return
	}
	row, err := h.store.UpdateResumeTweak(r.Context(), uid, id, in)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "tweak not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}

// DeleteResumeTweak handles DELETE /ai/resume/tweaks/{id}.
func (h *Handler) DeleteResumeTweak(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteResumeTweak(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "tweak not found")
			return
		}
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// optionalUUID parses a possibly-empty UUID string. Empty → nil, valid →
// pointer, otherwise writes 400 and returns (nil, false). Centralises
// the small repeated parse-or-bail dance.
func optionalUUID(w http.ResponseWriter, raw, fieldName string) (*uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid "+fieldName)
		return nil, false
	}
	return &id, true
}
