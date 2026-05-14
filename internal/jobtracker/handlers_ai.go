package jobtracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/TheBharathProject/sypher-api/internal/billing"
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

// gateAICredit enforces the two-tier billing model used by every paid
// AI endpoint:
//
//  1. Free monthly token quota (AI_USAGE_MONTHLY_TOKEN_LIMIT, default
//     25,000). Until this is exhausted, the call is free.
//  2. Paid credits balance. Once the free quota is exhausted, debit
//     `cost` credits via billing.Store.SpendCredits.
//
// Returns:
//   - "free" when the free quota covers the call (no credits debited).
//   - "credits" when free quota was exhausted and `cost` was debited.
//   - error otherwise. The handler writes the appropriate response and
//     returns; the AI call must NOT proceed.
//
// Callers MUST gate before any Deepseek call so over-quota users get a
// clean 402 without burning tokens. The free-quota usage is still
// recorded after a successful AI call via aiUsage.Record — credits
// debiting happens inside this gate, atomically.
func (h *Handler) gateAICredit(w http.ResponseWriter, r *http.Request, uid uuid.UUID, cost int, reason string) (mode string, ok bool) {
	err := h.aiUsage.EnforceLimit(r.Context(), uid)
	if err == nil {
		return "free", true
	}
	if !errors.Is(err, ai.ErrUsageExceeded) {
		h.logger.Error("ai usage check", "err", err, "user_id", uid)
		httpx.WriteError(w, http.StatusInternalServerError, "usage_check_failed", err.Error())
		return "", false
	}

	// Free quota exhausted. Try to debit credits.
	if h.billingStore == nil {
		// Billing not configured — fall back to the historical behaviour:
		// 429 once free quota is gone. Lets dev deployments without
		// Razorpay keep working.
		httpx.WriteError(w, http.StatusTooManyRequests, "usage_exceeded",
			"monthly AI usage limit exceeded")
		return "", false
	}

	if _, err := h.billingStore.SpendCredits(r.Context(), uid, cost, reason, "ai", uuid.Nil); err != nil {
		if errors.Is(err, billing.ErrInsufficientCredits) {
			httpx.WriteJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":    "insufficient_credits",
				"message":  "free monthly tokens exhausted; not enough credits to cover this call",
				"cost":     cost,
				"reason":   reason,
				"topUpURL": "/upgrade#credits",
			})
			return "", false
		}
		h.logger.Error("spend credits", "err", err, "user_id", uid, "cost", cost, "reason", reason)
		httpx.WriteError(w, http.StatusInternalServerError, "credits_debit_failed", err.Error())
		return "", false
	}
	return "credits", true
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
	// Exactly one of FileID, DraftID, or Text must be provided. FileID
	// pulls a Vault PDF, DraftID renders a Resume Builder draft to text
	// via the structured template, and Text is the user pasting raw.
	FileID         string `json:"fileId"`
	DraftID        string `json:"draftId"`
	Text           string `json:"text"`
	Level          string `json:"level"`
	TargetRole     string `json:"targetRole"`
	JobDescription string `json:"jobDescription"`
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
	var (
		fileID      *uuid.UUID
		draftID     *uuid.UUID
		latexSource string // populated only on draft path
		pdfMetaStr  string // populated only on fileId path with extractable metadata
	)

	switch {
	case resumeText != "":
		// Raw-text path — no DB read needed, no source available.
	case in.DraftID != "":
		// Builder draft path: render the draft's content through the
		// classic-v2 template. Feed the AI BOTH the plain-text view
		// (so it sees content the way an ATS would) AND the .tex source
		// (so it can evaluate formatting-dependent checklist items).
		did, err := uuid.Parse(in.DraftID)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid draftId")
			return
		}
		draftID = &did
		draft, err := h.store.GetResumeBuilderDraft(r.Context(), uid, did)
		if err != nil {
			writeDBError(w, err)
			return
		}
		tex, err := RenderResumeBuilderTeX(draft.Content, draft.TemplateID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "render_failed", err.Error())
			return
		}
		resumeText = stripTeXToPlainText(tex)
		latexSource = tex
	case in.FileID != "":
		// Vault PDF path — fetch from R2 and extract text.
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
		// Best-effort structural metadata: gives the AI cues to
		// evaluate formatting-dependent checklist items
		// (single_column, font_consistency, bolding_policy,
		// ats_safe_bullets, length_correct, etc.) that would
		// otherwise return "na" on uploaded PDFs.
		if file.MimeType == "application/pdf" || strings.HasSuffix(strings.ToLower(file.FileName), ".pdf") {
			if meta := ExtractPDFMetadata(body); meta != nil {
				pdfMetaStr = meta.String()
			}
		}
	default:
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fileId, draftId, or text required")
		return
	}

	if strings.TrimSpace(resumeText) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "empty_resume", "could not extract any text")
		return
	}

	if _, ok := h.gateAICredit(w, r, uid, billing.CostResumeReport, billing.ReasonResumeReport); !ok {
		return
	}

	// Async pattern: reserve the row id immediately, return 202, and
	// run the AI call in a detached goroutine. The FE navigates to
	// /resume?id=<id> right away and polls every 10s until the JSON
	// body lands. If the user wanders off, we drop a notification
	// when the work completes so they can come back to it.
	reportID, err := h.store.CreatePendingScoreReport(r.Context(), uid, fileID, draftID)
	if err != nil {
		writeDBError(w, err)
		return
	}

	// Detached context: the goroutine outlives this request. The AI
	// client's 90s HTTP timeout (deepseek.go) keeps the goroutine
	// bounded; nothing else can hang it.
	go h.runScoreReportAsync(uid, reportID, resumeText, latexSource, pdfMetaStr, in.Level, in.TargetRole, in.JobDescription, draftID, fileID)

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id":     reportID.String(),
		"status": "pending",
		"format": "pending",
	})
}

// runScoreReportAsync is the goroutine body for an async score job:
// run the AI call → validate → update the pending row → push a
// notification so the user finds the report even if they navigated
// away from /resume?id=<id>.
//
// On failure we leave the pending row in place + log; a future sweeper
// can mark rows older than N minutes as failed. Caller's request
// context is intentionally NOT used — the goroutine needs to outlive
// the request.
func (h *Handler) runScoreReportAsync(
	uid, reportID uuid.UUID,
	resumeText, latexSource, pdfMetaStr, level, targetRole, jobDescription string,
	draftID, fileID *uuid.UUID,
) {
	// Bound the goroutine independently — AI's 90s + our slack.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	report, usage, err := h.ai.ResumeScoreReportV2(ctx, resumeText, latexSource, pdfMetaStr, level, targetRole, jobDescription)
	if err != nil {
		h.logger.Error("ai score report v2 (async)", "err", err, "report_id", reportID, "user_id", uid)
		return
	}
	if usage != nil {
		if rErr := h.aiUsage.Record(ctx, uid, "resume_score_report", usage.TokensIn, usage.TokensOut); rErr != nil {
			h.logger.Warn("ai usage record (async)", "err", rErr, "report_id", reportID)
		}
	}

	report = ai.ValidateScoreReport(report, h.logger)
	reportJSON, mErr := json.Marshal(report)
	if mErr != nil {
		h.logger.Error("marshal score report (async)", "err", mErr, "report_id", reportID)
		return
	}
	if uErr := h.store.UpdateScoreReport(ctx, uid, reportID, reportJSON, report.OverallScore); uErr != nil {
		h.logger.Error("update score report (async)", "err", uErr, "report_id", reportID)
		return
	}

	// Notify the user so they can find the finished report if they
	// navigated away from /resume?id=<id>. The link path is the same
	// the FE uses for the redirect, and the FE notifications panel
	// already handles in-app deep links.
	if h.notifier != nil {
		// link_path is basePath-relative. The FE's router.push()
		// prepends `/pegasus` via Next.js basePath config — emitting
		// `/pegasus/resume?id=…` here would result in
		// /pegasus/pegasus/resume?id=…. Keep this in sync with the
		// FE strip logic in components/activity/notifications-pane.tsx.
		link := "/resume?id=" + reportID.String()
		refType := "ai_resume_report"
		_, nErr := h.notifier.Push(ctx, NotificationInput{
			UserID:   uid,
			Kind:     "ai_resume_report",
			RefType:  &refType,
			RefID:    &reportID,
			Title:    "Resume score is ready",
			Body:     fmt.Sprintf("Overall %d / 100. Tap to view the breakdown.", report.OverallScore),
			LinkPath: &link,
		})
		if nErr != nil {
			h.logger.Warn("notifier push score-ready", "err", nErr, "report_id", reportID)
		}
	}

	h.logger.Info("async score report complete", "report_id", reportID, "user_id", uid, "score", report.OverallScore)
}

// stripTeXToPlainText is a best-effort plain-text extractor for our
// own classic-v2 template output — fed to the AI so it sees text
// instead of `\textbf{}` macros. Not a general LaTeX parser; targets
// only the macros classic-v2 emits.
func stripTeXToPlainText(tex string) string {
	s := tex

	// Strip preamble + closing — keep only what's between
	// \begin{document} and \end{document}.
	if i := strings.Index(s, "\\begin{document}"); i >= 0 {
		s = s[i+len("\\begin{document}"):]
	}
	if i := strings.Index(s, "\\end{document}"); i >= 0 {
		s = s[:i]
	}

	// Replace argument-bearing macros with their argument: \section*{X}
	// → X; \textbf{X} → X; \textit{X} → X; \href{url}{label} → label.
	for _, name := range []string{"section\\*", "section", "textbf", "textit", "Large", "large", "Huge"} {
		re := regexp.MustCompile(`\\` + name + `\{([^}]*)\}`)
		s = re.ReplaceAllString(s, "$1")
	}
	// \href{url}{label} — keep the label only.
	s = regexp.MustCompile(`\\href\{[^}]*\}\{([^}]*)\}`).ReplaceAllString(s, "$1")

	// Drop visual-only macros wholesale.
	for _, m := range []string{
		`\\begin\{center\}`, `\\end\{center\}`,
		`\\begin\{itemize\}`, `\\end\{itemize\}`,
		`\\noindent`, `\\hfill`, `\\vspace\{[^}]*\}`,
		`\\\\(\[[^\]]*\])?`, // \\, \\[2pt], etc.
		`\\titlerule`, `\\color\{[^}]*\}`,
	} {
		s = regexp.MustCompile(m).ReplaceAllString(s, " ")
	}

	// \item → bullet on its own line so the AI sees per-bullet structure.
	s = regexp.MustCompile(`\\item\s*`).ReplaceAllString(s, "\n• ")

	// Inverse-escape the texEscape set.
	for _, sub := range [][2]string{
		{`\\textbackslash\{\}`, `\`},
		{`\\textasciitilde\{\}`, `~`},
		{`\\textasciicircum\{\}`, `^`},
		{`\\&`, `&`}, {`\\%`, `%`}, {`\\\$`, `$`},
		{`\\#`, `#`}, {`\\_`, `_`}, {`\\\{`, `{`}, {`\\\}`, `}`},
	} {
		s = regexp.MustCompile(sub[0]).ReplaceAllString(s, sub[1])
	}

	// Collapse runs of whitespace + blank lines.
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

var scoreRe = regexp.MustCompile("`(\\d{1,3})`")

// parseScore — legacy Markdown-score parser. Kept for any future caller
// that still wants to read a score out of free-form text.
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

// ListResumeReports — GET /job-tracker/ai/resume/reports
//
// Query params (all optional):
//
//	cursor=<rfc3339> page after this createdAt (exclusive)
//	limit=<n>        clamp to [1, 100], default 50
//
// Response: { items: AIReportSummary[], nextCursor: "..." | null }
// Markdown bodies are NOT returned here — see GetResumeReport for drill-in.
func (h *Handler) ListResumeReports(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())

	opts := ListAIReportsOpts{}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		t, err := time.Parse(time.RFC3339, cursor)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_cursor", "cursor must be RFC3339")
			return
		}
		opts.Cursor = t
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}

	items, lastTS, err := h.store.ListAIReports(r.Context(), uid, opts)
	if err != nil {
		writeDBError(w, err)
		return
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	var nextCursor string
	if len(items) == limit && !lastTS.IsZero() {
		nextCursor = lastTS.UTC().Format(time.RFC3339)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"nextCursor": nilIfBlank(nextCursor),
	})
}

// GetResumeReport — GET /job-tracker/ai/resume/reports/{id}
//
// Returns the full AIReport (including markdown) for a single report owned
// by the caller. 404 if the id doesn't exist or isn't owned.
func (h *Handler) GetResumeReport(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid report id")
		return
	}
	rep, err := h.store.GetReport(r.Context(), uid, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "report not found")
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

	if _, ok := h.gateAICredit(w, r, uid, billing.CostCoverLetter, billing.ReasonCoverLetter); !ok {
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
