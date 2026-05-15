package jobtracker

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// ============================================================================
// Resume Builder — LaTeX rendering.
//
// Two responsibilities:
//
//   1. RenderResumeBuilderTeX: take a DraftContent, return a .tex string.
//      Pure function — no I/O. Used by the /render/tex endpoint for power
//      users + as the input to RenderResumeBuilderPDF.
//
//   2. RenderResumeBuilderPDF: POST the .tex to a sidecar HTTP service
//      (yotech/latex-on-http or compatible) and return the PDF bytes.
//      The service runs full TeX Live in its own container — gives us
//      reliability against arbitrary user-imported LaTeX without bloating
//      this binary's image. URL comes from Handler.latexServiceURL,
//      sourced from LATEX_SERVICE_URL env var.
//
// LaTeX-special characters in user input are escaped via texEscape before
// substitution into the template — see template func "esc". Without that,
// a literal '%' in a description would silently truncate the bullet (LaTeX
// treats % as a comment start) and '&' would error the compile entirely.
// ============================================================================

//go:embed templates/resume_builder/classic.tex.tmpl
var resumeBuilderClassicTmpl string

//go:embed templates/resume_builder/classic-v2.tex.tmpl
var resumeBuilderClassicV2Tmpl string

// classic-v2 is the parse-friendly variant. The FE template parser at
// job-tracker/lib/resume-builder/templates/classic-v2.ts depends on the
// exact macros emitted by this template — see the invariants comment at
// the top of the .tmpl file before tweaking it.
var resumeBuilderTemplates = map[string]*template.Template{
	"classic-v1": mustParseResumeBuilderTemplate(resumeBuilderClassicTmpl),
	"classic-v2": mustParseResumeBuilderTemplate(resumeBuilderClassicV2Tmpl),
}

func mustParseResumeBuilderTemplate(src string) *template.Template {
	funcs := template.FuncMap{
		"esc":         texEscape,
		"escJoin":     texEscapeJoin,
		"contactLine": renderContactLine,
		"dateRange":   renderDateRange,
		"accentHex":   accentHex,
	}
	t, err := template.New("resume").Funcs(funcs).Parse(src)
	if err != nil {
		// At package init — panic is the right call. Bad template = bad build.
		panic("resume builder: parse template: " + err.Error())
	}
	return t
}

// normalizeStyleFontFamily maps an FE-supplied font id to a known value
// the template branches on. Legacy "serif" / "sans" become the matching
// new IDs; anything unknown falls through to the empty string, which
// the template treats as the default (Latin Modern Roman). Mirrors
// job-tracker/lib/resume-builder/font-registry.ts:normalizeFontFamily.
func normalizeStyleFontFamily(s string) string {
	switch strings.TrimSpace(s) {
	case "sans":
		return "helvetica"
	case "serif", "":
		return "lmodern"
	case "lmodern", "helvetica", "times", "palatino", "charter", "ebgaramond":
		return s
	default:
		return "lmodern"
	}
}

// accentHex normalises the FE-supplied colour to the bare 6-char hex
// xcolor's \definecolor expects. Accepts "#1a1a1a" / "1a1a1a" / empty
// (falls back to dark grey). Anything else is rejected back to the
// default so we never feed garbage to LaTeX.
func accentHex(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return "1a1a1a"
	}
	for _, r := range s {
		ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !ok {
			return "1a1a1a"
		}
	}
	return s
}

// errLatexServiceUnavailable is returned when the sidecar URL isn't set
// OR when the HTTP call fails with a network error (container down,
// DNS fail, refused, etc.). The calling handler maps it to 503 so the
// FE can fall back to showing the raw .tex source.
var errLatexServiceUnavailable = errors.New("latex compile service is not configured or unreachable")

// RenderResumeBuilderTeX produces a LaTeX source string for the given draft
// content + template id. Returns an error if the template id is unknown.
//
// If content.CustomTeX is set, it bypasses the template entirely and returns
// that string verbatim — this is the path the editor's "LaTeX source" mode
// uses to let a power user ship a hand-edited .tex through the export
// pipeline without losing their structured form data.
func RenderResumeBuilderTeX(content DraftContent, templateID string) (string, error) {
	if content.CustomTeX != "" {
		return content.CustomTeX, nil
	}
	if templateID == "" {
		templateID = defaultResumeBuilderTemplate
	}
	tmpl, ok := resumeBuilderTemplates[templateID]
	if !ok {
		return "", fmt.Errorf("unknown template %q", templateID)
	}
	// Normalize the font choice so the template only sees known IDs.
	// Legacy "serif"/"sans" drafts and anything malformed map to a
	// known package; the template's else-branch handles the default.
	content.Style.FontFamily = normalizeStyleFontFamily(content.Style.FontFamily)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, content); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}

// RenderResumeBuilderPDF posts the .tex to the configured LaTeX sidecar
// (yotech/latex-on-http or compatible) and returns the rendered PDF
// bytes. serviceURL is the base URL of the sidecar (e.g.
// "http://sypher-tex" in prod over the Docker network, or
// "http://localhost:8090" in local dev). When serviceURL is empty,
// returns errLatexServiceUnavailable so the handler can 503.
//
// The HTTP contract is yotech's /builds/sync — POST a JSON body, get
// either a 200 with the PDF or a 4xx with a JSON envelope containing
// the compile log. We bound the call at 60s and surface compile-log
// tails on failure so the FE's existing <pre> block can display them.
func RenderResumeBuilderPDF(ctx context.Context, serviceURL, tex string) ([]byte, error) {
	if strings.TrimSpace(serviceURL) == "" {
		return nil, errLatexServiceUnavailable
	}

	// Default to pdflatex — it handles the bulk of real-world resume
	// templates (including ones that \input{glyphtounicode} or use
	// other pdfTeX-specific primitives). xelatex is only required for
	// Unicode-heavy documents using fontspec / system fonts, which
	// are the minority for ATS-friendly resumes. We can expose the
	// choice in the FE later if users need xelatex for specific docs.
	reqBody, err := json.Marshal(map[string]any{
		"compiler": "pdflatex",
		"resources": []map[string]any{
			{"main": true, "content": tex},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build compile request: %w", err)
	}

	// 60s covers cold-start (font cache) + complex docs. Generous but
	// bounded so a runaway compile can't pin a request goroutine.
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	endpoint := strings.TrimRight(serviceURL, "/") + "/builds/sync"
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build compile request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/pdf, application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Distinguish "timed out" from "couldn't reach the service" so the
		// user-facing message is accurate. Timeout = compiler is up but
		// slow / hung; non-timeout = network/DNS/container-down.
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("latex compile timed out after 60s")
		}
		return nil, fmt.Errorf("%w: %v", errLatexServiceUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		pdfBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read compile response: %w", err)
		}
		// The sidecar has been observed to return 200 + an empty body
		// (and 200 + a non-PDF body) when pdflatex "succeeds" but produces
		// no pages — e.g. an empty document with hyperref loaded creates
		// a 0-byte resume.pdf that satisfies the existence check inside
		// the container. Validate the magic bytes here so the FE never
		// receives an empty/garbled application/pdf response.
		if len(pdfBytes) < 5 || !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
			return nil, fmt.Errorf("compile failed\n\nthe LaTeX produced no pages — check that your document has content between \\begin{document} and \\end{document}")
		}
		return pdfBytes, nil
	}

	// Decode yotech's error envelope. Both fields are best-effort — the
	// service has been seen to return either or both.
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Logs  string `json:"logs"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	// 4xx = the LaTeX itself failed — return the compile log so the FE
	// can render the existing <pre class="rb-tex-preview-log"> block.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		logs := strings.TrimSpace(env.Logs)
		if logs == "" {
			logs = strings.TrimSpace(env.Error)
		}
		if logs == "" {
			// Service returned 4xx but no log/error text — fall back to a
			// short string so we don't show the FE an empty body.
			logs = fmt.Sprintf("compile failed (HTTP %d, no log returned by service)", resp.StatusCode)
		}
		return nil, fmt.Errorf("compile failed\n\n%s", tailLines(logs, 60))
	}

	// 5xx = the service itself is unhappy. Treat as unavailable so the
	// FE shows the "compile service is offline" fallback.
	hint := strings.TrimSpace(env.Error)
	if hint == "" {
		hint = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("%w: %s", errLatexServiceUnavailable, hint)
}

// ----------------------------------------------------------------------------
// Template helpers
// ----------------------------------------------------------------------------

// texEscape escapes LaTeX special characters in user input. Order matters —
// the backslash replacement has to run first so subsequent inserts don't
// double-escape. The replacement of \ -> \textbackslash{} uses a placeholder
// (\\\\BS\\\\) so we don't repeatedly re-substitute.
var texReplacer = strings.NewReplacer(
	`\`, `\textbackslash{}`,
	`&`, `\&`,
	`%`, `\%`,
	`$`, `\$`,
	`#`, `\#`,
	`_`, `\_`,
	`{`, `\{`,
	`}`, `\}`,
	`~`, `\textasciitilde{}`,
	`^`, `\textasciicircum{}`,
)

func texEscape(s string) string {
	return texReplacer.Replace(s)
}

// texEscapeJoin escapes each element and joins with a comma+space.
// Used for the Skills section where each group has an []string of items.
func texEscapeJoin(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := make([]string, 0, len(items))
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, texEscape(s))
	}
	return strings.Join(out, ", ")
}

// renderContactLine produces the centred contact line under the name —
// "email · phone · location · linkedin · github · website" with non-empty
// fields only. Links use \href so they're clickable AND text-extractable.
func renderContactLine(p DraftPersonal) string {
	var parts []string
	if p.Email != "" {
		parts = append(parts, `\href{mailto:`+p.Email+`}{`+texEscape(p.Email)+`}`)
	}
	if p.Phone != "" {
		parts = append(parts, texEscape(p.Phone))
	}
	if p.Location != "" {
		parts = append(parts, texEscape(p.Location))
	}
	if p.LinkedinURL != "" {
		parts = append(parts, `\href{`+p.LinkedinURL+`}{LinkedIn}`)
	}
	if p.GithubURL != "" {
		parts = append(parts, `\href{`+p.GithubURL+`}{GitHub}`)
	}
	if p.WebsiteURL != "" {
		parts = append(parts, `\href{`+p.WebsiteURL+`}{Website}`)
	}
	// LaTeX `\cdot` looks better than a plain bullet between contact items.
	return strings.Join(parts, ` $\cdot$ `)
}

// tailLines returns the last n newline-separated lines of s. Used to bound
// Tectonic's potentially-long compile log when bubbling it back to the FE —
// the failing line is almost always near the end, so the head can go.
func tailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// renderDateRange produces "Jan 2024 – Present" / "Jan 2024 – Dec 2024" /
// "Jan 2024" / "" depending on which dates are populated. en-dash is the
// typographically correct separator for date ranges.
func renderDateRange(start, end string, current bool) string {
	start = strings.TrimSpace(start)
	end = strings.TrimSpace(end)
	startEsc := texEscape(start)
	endEsc := texEscape(end)
	switch {
	case start == "" && end == "" && !current:
		return ""
	case current:
		if start == "" {
			return "Present"
		}
		return startEsc + " -- Present"
	case start == "":
		return endEsc
	case end == "":
		return startEsc
	default:
		return startEsc + " -- " + endEsc
	}
}
