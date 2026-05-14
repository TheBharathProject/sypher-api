package jobtracker

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/TheBharathProject/sypher-api/internal/ai"
)

//go:embed templates/ai_report/score_report.tex.tmpl
var scoreReportTmplSrc string

var scoreReportTmpl = mustParseScoreReportTemplate(scoreReportTmplSrc)

func mustParseScoreReportTemplate(src string) *template.Template {
	funcs := template.FuncMap{
		"esc":                  texEscape,
		"tagMacro":             scoreReportTagMacro,
		"checklistStatusMacro": checklistStatusMacro,
		"verdictLabel":         func(p ai.VerdictPile) string { return ai.VerdictPileLabel(p) },
		"verdictColour":        verdictColour,
		"barWidth":             barWidth,
		"barTrackWidth":        barTrackWidth,
	}
	t, err := template.New("score-report").Funcs(funcs).Parse(src)
	if err != nil {
		panic("score report: parse template: " + err.Error())
	}
	return t
}

// scoreReportTagMacro maps a section finding's tag to the corresponding
// LaTeX macro invocation. Trailing `{}` is a zero-width brace group that
// terminates the control-sequence name — without it LaTeX would parse
// `\tagfindingGoodThe...` as one undefined macro.
func scoreReportTagMacro(tag ai.FindingTag) string {
	switch tag {
	case ai.FindingFix:
		return `\tagfindingFix{}`
	case ai.FindingImprove:
		return `\tagfindingImprove{}`
	case ai.FindingGood:
		return `\tagfindingGood{}`
	default:
		return ""
	}
}

// checklistStatusMacro maps a checklist status to its LaTeX macro.
// Same {} idiom as scoreReportTagMacro.
func checklistStatusMacro(s ai.ChecklistStatus) string {
	switch s {
	case ai.ChecklistPass:
		return `\statusPass{}`
	case ai.ChecklistWarn:
		return `\statusWarn{}`
	case ai.ChecklistFail:
		return `\statusFail{}`
	case ai.ChecklistNA:
		return `\statusNa{}`
	default:
		return ""
	}
}

// verdictColour picks an xcolor name defined in the template preamble.
func verdictColour(p ai.VerdictPile) string {
	switch p {
	case ai.VerdictYes:
		return "verdictYes"
	case ai.VerdictMaybe:
		return "verdictMaybe"
	case ai.VerdictNo:
		return "verdictNo"
	default:
		return "muted"
	}
}

// barWidth + barTrackWidth render the section-score bar chart. The
// chart bar is a max of 8 cm wide; score% × 8cm fills, the rest is
// rendered in a track colour to keep alignment.
const barMaxCm = 8.0

func barWidth(score int) string {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	w := float64(score) / 100.0 * barMaxCm
	return fmt.Sprintf("%.2f", w)
}

func barTrackWidth(score int) string {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	w := (100.0 - float64(score)) / 100.0 * barMaxCm
	return fmt.Sprintf("%.2f", w)
}

// scoreReportSectionView is the per-section view passed to the
// template. Wraps ai.ScoreSection with display fields the template
// needs (human label, counts string).
type scoreReportSectionView struct {
	Label    string
	Score    int
	Counts   string
	Findings []ai.ScoreFinding
}

// checklistGroupView wraps a checklist group for the PDF — the model
// emits a flat list with group keys; we re-bucket here in canonical
// group order so the rendering is deterministic.
type checklistGroupView struct {
	Label string
	Items []ai.ChecklistItem
}

// coreDiagnosisView is the template-facing shape for CoreDiagnosis.
// Pre-renders human labels for IdentityMatch + Archetype so the
// template doesn't need helper funcs for those two enums.
type coreDiagnosisView struct {
	SixSecondVerdict string
	IdentityLabel    string
	ArchetypeLabel   string
	HiddenStory      string
	CoreProblem      string
}

// scoreReportTemplateData is the root context for the LaTeX template.
type scoreReportTemplateData struct {
	IsV2             bool
	OverallScore     int
	AtsScore         int
	VerdictPile      ai.VerdictPile
	VerdictTagline   string
	CoreDiagnosis    *coreDiagnosisView
	ExecutiveSummary string
	Sections         []scoreReportSectionView
	Checklist        []checklistGroupView
	ImprovementPlan  []ai.ImprovementPlanItem
}

// buildScoreReportTemplateData converts the wire-format ScoreReport
// into the template's render shape, preserving SectionOrder so the PDF
// section sequence matches the FE nav. Detects v1 vs v2 so the
// template can skip v2-only blocks when reading legacy rows.
func buildScoreReportTemplateData(r *ai.ScoreReport) scoreReportTemplateData {
	out := scoreReportTemplateData{
		IsV2:             r.IsV2(),
		OverallScore:     r.OverallScore,
		AtsScore:         r.AtsScore,
		VerdictPile:      r.VerdictPile,
		VerdictTagline:   r.VerdictTagline,
		ExecutiveSummary: r.ExecutiveSummary,
	}
	for _, key := range ai.SectionOrder {
		sec, ok := r.Sections[key]
		if !ok {
			out.Sections = append(out.Sections, scoreReportSectionView{
				Label: ai.SectionLabel(key),
				Score: 0,
			})
			continue
		}
		out.Sections = append(out.Sections, scoreReportSectionView{
			Label:    ai.SectionLabel(key),
			Score:    sec.Score,
			Counts:   countsSummary(sec.Findings),
			Findings: sec.Findings,
		})
	}

	if out.IsV2 {
		out.Checklist = buildChecklistGroups(r.Checklist)
		out.ImprovementPlan = r.ImprovementPlan
		if r.CoreDiagnosis != nil {
			out.CoreDiagnosis = &coreDiagnosisView{
				SixSecondVerdict: r.CoreDiagnosis.SixSecondVerdict,
				IdentityLabel:    identityMatchLabel(r.CoreDiagnosis.IdentityMatch),
				ArchetypeLabel:   ai.ArchetypeLabel(r.CoreDiagnosis.Archetype),
				HiddenStory:      r.CoreDiagnosis.HiddenStory,
				CoreProblem:      r.CoreDiagnosis.CoreProblem,
			}
		}
	}
	return out
}

func identityMatchLabel(m ai.IdentityMatch) string {
	switch m {
	case ai.IdentityMatchYes:
		return "match"
	case ai.IdentityMatchNo:
		return "mismatch"
	case ai.IdentityMatchUnclear:
		return "unclear"
	}
	return ""
}

// buildChecklistGroups re-buckets the flat checklist into canonical
// groups in display order. Items the validator already coerced to a
// known group are placed into their group; unknown items (defensive)
// land in an "Other" group at the end.
func buildChecklistGroups(items []ai.ChecklistItem) []checklistGroupView {
	byGroup := map[string][]ai.ChecklistItem{}
	for _, it := range items {
		byGroup[it.Group] = append(byGroup[it.Group], it)
	}
	out := []checklistGroupView{}
	for _, g := range []struct {
		key   string
		label string
	}{
		{ai.GroupCommonMistakes, "Common Mistakes"},
		{ai.GroupDosDonts, "Do's and Don'ts"},
		{ai.GroupStandsOut, "Stands Out"},
	} {
		if rows, ok := byGroup[g.key]; ok && len(rows) > 0 {
			out = append(out, checklistGroupView{Label: g.label, Items: rows})
			delete(byGroup, g.key)
		}
	}
	for k, rows := range byGroup {
		out = append(out, checklistGroupView{Label: strings.ToValidUTF8(k, "?"), Items: rows})
	}
	return out
}

// countsSummary renders a "N to fix · N to improve · N good" string —
// matches the on-screen header for each section.
func countsSummary(findings []ai.ScoreFinding) string {
	var fix, imp, good int
	for _, f := range findings {
		switch f.Tag {
		case ai.FindingFix:
			fix++
		case ai.FindingImprove:
			imp++
		case ai.FindingGood:
			good++
		}
	}
	parts := []string{}
	if fix > 0 {
		parts = append(parts, fmt.Sprintf("%d to fix", fix))
	}
	if imp > 0 {
		parts = append(parts, fmt.Sprintf("%d to improve", imp))
	}
	if good > 0 {
		parts = append(parts, fmt.Sprintf("%d good", good))
	}
	return strings.Join(parts, "  ·  ")
}

// RenderScoreReportTeX produces the LaTeX source for a Resume Score
// report. Pure function — no I/O.
func RenderScoreReportTeX(r *ai.ScoreReport) (string, error) {
	if r == nil {
		return "", fmt.Errorf("score report: nil report")
	}
	data := buildScoreReportTemplateData(r)
	var buf bytes.Buffer
	if err := scoreReportTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("score report: render template: %w", err)
	}
	return buf.String(), nil
}

// RenderScoreReportPDF renders the report to LaTeX and compiles it
// via the configured sypher-tex sidecar. Returns the PDF bytes.
// Reuses the existing RenderResumeBuilderPDF path — same HTTP
// contract, same error shape, same magic-byte validation.
func RenderScoreReportPDF(ctx context.Context, serviceURL string, r *ai.ScoreReport) ([]byte, error) {
	tex, err := RenderScoreReportTeX(r)
	if err != nil {
		return nil, err
	}
	return RenderResumeBuilderPDF(ctx, serviceURL, tex)
}
