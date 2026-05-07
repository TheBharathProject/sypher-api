package jobtracker

import (
	"bytes"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/ledongthuc/pdf"

	"github.com/TheBharathProject/sypher-api/internal/ai"
	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

func (h *Handler) requireAI(w http.ResponseWriter) bool {
	if h.ai == nil || h.aiUsage == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "ai_unavailable",
			"AI features are not configured on this deployment")
		return false
	}
	return true
}

// ----------------------------------------------------------------------------
// /resume/extract — pulls bytes from R2, extracts text (PDF) or returns raw.
// ----------------------------------------------------------------------------

type extractInput struct {
	FileID string `json:"fileId"`
	Text   string `json:"text"` // optional override; bypasses storage if set
}

type extractResult struct {
	Text string `json:"text"`
}

func (h *Handler) ExtractResume(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in extractInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Text != "" {
		httpx.WriteJSON(w, http.StatusOK, extractResult{Text: in.Text})
		return
	}
	if in.FileID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fileId or text is required")
		return
	}
	if !h.requireR2(w) {
		return
	}
	id, err := uuid.Parse(in.FileID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid fileId")
		return
	}
	file, err := h.store.GetFile(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	body, err := h.r2.FetchAll(r.Context(), file.StorageKey)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	text := extractText(body, file.MimeType, file.FileName)
	httpx.WriteJSON(w, http.StatusOK, extractResult{Text: text})
}

// extractText is best-effort: returns the file as UTF-8 if it's plain text,
// pulls text from a PDF if it parses, otherwise returns "". DOCX support is
// deferred — recommend the user paste plain text.
func extractText(body []byte, mime, name string) string {
	lower := strings.ToLower(name)
	if mime == "application/pdf" || strings.HasSuffix(lower, ".pdf") {
		return extractPDF(body)
	}
	// Heuristic: assume txt/md/anything else valid UTF-8
	return string(body)
}

func extractPDF(data []byte) string {
	r := bytes.NewReader(data)
	doc, err := pdf.NewReader(r, int64(len(data)))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	totalPages := doc.NumPage()
	for i := 1; i <= totalPages; i++ {
		page := doc.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// ----------------------------------------------------------------------------
// /ai/resume/report
// ----------------------------------------------------------------------------

type reportInput struct {
	FileID string `json:"fileId"`
	Text   string `json:"text"`
}

func (h *Handler) GenerateResumeReport(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	if !h.requireAI(w) {
		return
	}
	var in reportInput
	if !readJSON(w, r, &in) {
		return
	}
	resumeText := strings.TrimSpace(in.Text)
	var fileID *uuid.UUID
	if resumeText == "" {
		if in.FileID == "" {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fileId or text required")
			return
		}
		if !h.requireR2(w) {
			return
		}
		fid, err := uuid.Parse(in.FileID)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid fileId")
			return
		}
		fileID = &fid
		file, err := h.store.GetFile(r.Context(), uid, fid)
		if err != nil {
			writeDBError(w, err)
			return
		}
		body, err := h.r2.FetchAll(r.Context(), file.StorageKey)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "fetch_failed", err.Error())
			return
		}
		resumeText = extractText(body, file.MimeType, file.FileName)
	}
	if strings.TrimSpace(resumeText) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "empty_resume", "could not extract any text")
		return
	}

	if err := h.aiUsage.EnforceLimit(r.Context(), uid); err != nil {
		if errors.Is(err, ai.ErrUsageExceeded) {
			httpx.WriteError(w, http.StatusTooManyRequests, "usage_exceeded",
				"monthly AI usage limit exceeded")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "usage_check_failed", err.Error())
		return
	}

	res, err := h.ai.ResumeReport(r.Context(), resumeText)
	if err != nil {
		h.logger.Error("ai report", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "ai_failed", err.Error())
		return
	}
	if err := h.aiUsage.Record(r.Context(), uid, "resume_report", res.TokensIn, res.TokensOut); err != nil {
		h.logger.Warn("ai usage record", "err", err)
	}

	score := parseScore(res.Text)
	saved, err := h.store.SaveReport(r.Context(), uid, fileID, res.Text, score)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

var scoreRe = regexp.MustCompile("`(\\d{1,3})`")

func parseScore(md string) int {
	m := scoreRe.FindStringSubmatch(md)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || n > 100 {
		return 0
	}
	return n
}

// ----------------------------------------------------------------------------
// /ai/resume/report/latest
// ----------------------------------------------------------------------------

func (h *Handler) LatestResumeReport(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	rep, err := h.store.LatestReport(r.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "no report yet")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rep)
}

// ----------------------------------------------------------------------------
// /ai/cover-letter
// ----------------------------------------------------------------------------

type coverLetterInput struct {
	JobDescription string `json:"jobDescription"`
	ResumeText     string `json:"resumeText"`
	ResumeFileID   string `json:"resumeFileId"`
}

func (h *Handler) GenerateCoverLetter(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	if !h.requireAI(w) {
		return
	}
	var in coverLetterInput
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.JobDescription) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "jobDescription required")
		return
	}

	resumeText := strings.TrimSpace(in.ResumeText)
	if resumeText == "" && in.ResumeFileID != "" {
		if !h.requireR2(w) {
			return
		}
		fid, err := uuid.Parse(in.ResumeFileID)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid resumeFileId")
			return
		}
		file, err := h.store.GetFile(r.Context(), uid, fid)
		if err != nil {
			writeDBError(w, err)
			return
		}
		body, err := h.r2.FetchAll(r.Context(), file.StorageKey)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "fetch_failed", err.Error())
			return
		}
		resumeText = extractText(body, file.MimeType, file.FileName)
	}
	if strings.TrimSpace(resumeText) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "resumeText or resumeFileId required")
		return
	}

	if err := h.aiUsage.EnforceLimit(r.Context(), uid); err != nil {
		if errors.Is(err, ai.ErrUsageExceeded) {
			httpx.WriteError(w, http.StatusTooManyRequests, "usage_exceeded",
				"monthly AI usage limit exceeded")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "usage_check_failed", err.Error())
		return
	}

	res, err := h.ai.CoverLetter(r.Context(), in.JobDescription, resumeText)
	if err != nil {
		h.logger.Error("ai cover letter", "err", err)
		httpx.WriteError(w, http.StatusBadGateway, "ai_failed", err.Error())
		return
	}
	if err := h.aiUsage.Record(r.Context(), uid, "cover_letter", res.TokensIn, res.TokensOut); err != nil {
		h.logger.Warn("ai usage record", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"coverLetter": res.Text})
}

// ----------------------------------------------------------------------------
// /ai/usage
// ----------------------------------------------------------------------------

func (h *Handler) AIUsage(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	if h.aiUsage == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"used":        0,
			"limit":       0,
			"periodStart": "",
			"periodEnd":   "",
		})
		return
	}
	used, err := h.aiUsage.MonthlyTotal(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	start, end := ai.PeriodWindow(time.Now())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"used":        used,
		"limit":       h.aiUsage.Limit(),
		"periodStart": start.Format(time.RFC3339),
		"periodEnd":   end.Format(time.RFC3339),
	})
}
