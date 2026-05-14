package ai

// Structured score-report types used by the Resume AI flow. Returned by
// Client.ResumeScoreReportV2 and persisted as JSONB in
// job_tracker.ai_reports.report_json (migration 0024).
//
// Section keys are stable strings. Adding a new section means:
//   1. Add it to SectionOrder.
//   2. Reference it in the prompt schema (prompts_resume_score.go).
//   3. Render it in the FE section-nav + PDF template.
//
// v2 additions (Naukri-parity):
//   - ats_score: separate ATS-friendliness number 0-100
//   - verdict_pile: "yes" | "maybe" | "no" (derived bucket from overall)
//   - verdict_tagline: 1-line verdict
//   - checklist: ~42 audit items grouped (common_mistakes / dos_donts / stands_out)
//   - improvement_plan: prioritised actions with effort × impact + before/after
//
// v1 reports remain readable — every v2-only field is `omitempty` so an
// old report deserialises into a ScoreReport with the new fields blank.

type FindingTag string

const (
	FindingFix     FindingTag = "fix"
	FindingImprove FindingTag = "improve"
	FindingGood    FindingTag = "good"
)

type ScoreFinding struct {
	Tag  FindingTag `json:"tag"`
	Text string     `json:"text"`
}

type ScoreSection struct {
	Score    int            `json:"score"`
	Findings []ScoreFinding `json:"findings"`
}

// VerdictPile is the recruiter's three-bucket verdict on the resume.
// Derived from overall_score by the AI; validated by the backend
// (see score_report_validate.go).
type VerdictPile string

const (
	VerdictYes   VerdictPile = "yes"
	VerdictMaybe VerdictPile = "maybe"
	VerdictNo    VerdictPile = "no"
)

// ChecklistStatus is the per-item verdict for the audit checklist.
// `na` is reserved for checks the AI genuinely cannot evaluate from
// the input (e.g. formatting checks on a Vault PDF where the .tex
// source isn't available).
type ChecklistStatus string

const (
	ChecklistPass ChecklistStatus = "pass"
	ChecklistWarn ChecklistStatus = "warn"
	ChecklistFail ChecklistStatus = "fail"
	ChecklistNA   ChecklistStatus = "na"
)

// ChecklistGroup names — stable strings used to bucket items in the
// FE/PDF layouts. The set of groups is fixed; new groups require code
// updates in the renderer.
const (
	GroupCommonMistakes = "common_mistakes"
	GroupDosDonts       = "dos_donts"
	GroupStandsOut      = "stands_out"
)

// ChecklistItem is one row in the v2 audit. Stable `id` lets the FE
// render groups in canonical order regardless of model output order.
type ChecklistItem struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Group    string          `json:"group"`
	Status   ChecklistStatus `json:"status"`
	Evidence string          `json:"evidence"`
}

// Effort/Impact pills on improvement plan items. Constrained set so the
// FE can render them as discrete tags.
type Effort string
type Impact string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	ImpactLow    Impact = "low"
	ImpactMedium Impact = "medium"
	ImpactHigh   Impact = "high"
)

// ImprovementPlanItem mirrors the "#N — title · effort · impact"
// blocks on Naukri's report, with verbatim before/after text the
// candidate can copy-paste. v2.1 adds `Why` — the principle behind
// the rewrite so the candidate can generalise it to other bullets,
// borrowed from the resume-analyzer skill's "explain the principle"
// requirement.
type ImprovementPlanItem struct {
	Title  string `json:"title"`
	Where  string `json:"where"`
	Effort Effort `json:"effort"`
	Impact Impact `json:"impact"`
	Before string `json:"before"`
	After  string `json:"after"`
	Why    string `json:"why,omitempty"`
}

// IdentityMatch describes whether the resume's 6-second projected
// role-identity matches the candidate's target. Used by CoreDiagnosis.
type IdentityMatch string

const (
	IdentityMatchYes     IdentityMatch = "match"
	IdentityMatchNo      IdentityMatch = "mismatch"
	IdentityMatchUnclear IdentityMatch = "unclear"
)

// Archetype is the matched diagnostic archetype from the skill's
// playbook. Empty string when no archetype clearly applies (resume is
// in the B+ range with only incremental improvements needed).
type Archetype string

const (
	ArchetypeIdentityCrisis      Archetype = "identity_crisis"
	ArchetypeUnderseller         Archetype = "underseller"
	ArchetypeOverseller          Archetype = "overseller"
	ArchetypeListOfJobs          Archetype = "list_of_jobs"
	ArchetypeDutyLister          Archetype = "duty_lister"
	ArchetypeCareerChanger       Archetype = "career_changer"
	ArchetypeLongInTheTooth      Archetype = "long_in_the_tooth"
	ArchetypeJuniorLookingSenior Archetype = "junior_looking_senior"
	ArchetypeSeniorLookingJunior Archetype = "senior_looking_junior"
	ArchetypeToolLister          Archetype = "tool_lister"
)

// CoreDiagnosis is the diagnosis-first block borrowed from the
// resume-analyzer skill. Anchors the entire report: the 6-second
// recruiter verdict, whether it matches the candidate's target, the
// matched archetype (if any), the "hidden story" the candidate is
// almost certainly doing but didn't write down, and a one-sentence
// statement of the core problem.
//
// All fields are required when CoreDiagnosis is present, but the
// pointer itself is optional on ScoreReport — v1/v2 reports without
// diagnosis still deserialise cleanly.
type CoreDiagnosis struct {
	SixSecondVerdict string        `json:"six_second_verdict"`
	IdentityMatch    IdentityMatch `json:"identity_match"`
	Archetype        Archetype     `json:"archetype,omitempty"`
	HiddenStory      string        `json:"hidden_story"`
	CoreProblem      string        `json:"core_problem"`
}

// ArchetypeLabel renders the archetype as a user-facing string.
func ArchetypeLabel(a Archetype) string {
	switch a {
	case ArchetypeIdentityCrisis:
		return "Identity Crisis"
	case ArchetypeUnderseller:
		return "Underseller"
	case ArchetypeOverseller:
		return "Overseller"
	case ArchetypeListOfJobs:
		return "List of Jobs"
	case ArchetypeDutyLister:
		return "Duty Lister"
	case ArchetypeCareerChanger:
		return "Career Changer"
	case ArchetypeLongInTheTooth:
		return "Long in the Tooth"
	case ArchetypeJuniorLookingSenior:
		return "Junior Looking Senior"
	case ArchetypeSeniorLookingJunior:
		return "Senior Looking Junior"
	case ArchetypeToolLister:
		return "Tool Lister"
	}
	return ""
}

// ScoreReport is the full structured output for a single Resume Score
// generation. JSON keys are stable across the wire and in storage.
type ScoreReport struct {
	OverallScore     int                     `json:"overall_score"`
	AtsScore         int                     `json:"ats_score,omitempty"`
	VerdictPile      VerdictPile             `json:"verdict_pile,omitempty"`
	VerdictTagline   string                  `json:"verdict_tagline,omitempty"`
	CoreDiagnosis    *CoreDiagnosis          `json:"core_diagnosis,omitempty"`
	ExecutiveSummary string                  `json:"executive_summary"`
	Sections         map[string]ScoreSection `json:"sections"`
	Checklist        []ChecklistItem         `json:"checklist,omitempty"`
	ImprovementPlan  []ImprovementPlanItem   `json:"improvement_plan,omitempty"`
}

// IsV2 reports whether a parsed ScoreReport has the v2 fields. Used by
// the FE/PDF renderer to fall back to v1 layout when reading an old
// row. The detector intentionally requires BOTH ats_score and
// checklist — a row missing either is treated as v1.
func (r *ScoreReport) IsV2() bool {
	return r.AtsScore > 0 && len(r.Checklist) > 0
}

// SectionOrder is the canonical display order. v2 prepends "summary"
// so the absence/quality of an intro paragraph gets its own score.
var SectionOrder = []string{
	"summary",
	"work_experience",
	"education",
	"skills",
	"projects",
	"achievements_awards",
}

// SectionLabel returns a human-friendly label for a section key.
// Falls back to the raw key if unknown (defensive — keeps unknown
// sections visible instead of silently hiding them).
func SectionLabel(key string) string {
	switch key {
	case "summary":
		return "Summary"
	case "work_experience":
		return "Work Experience"
	case "education":
		return "Education"
	case "skills":
		return "Skills"
	case "projects":
		return "Projects"
	case "achievements_awards":
		return "Achievements / Awards"
	default:
		return key
	}
}

// VerdictPileLabel renders the verdict pile as a user-facing string
// matching Naukri's UI ("YES PILE" / "MAYBE PILE" / "NO PILE").
func VerdictPileLabel(p VerdictPile) string {
	switch p {
	case VerdictYes:
		return "YES PILE"
	case VerdictMaybe:
		return "MAYBE PILE"
	case VerdictNo:
		return "NO PILE"
	default:
		return ""
	}
}

// VerdictFromScore returns the canonical pile for a given overall
// score using the v2 thresholds (≥85 yes, 60-84 maybe, <60 no). Used
// by the validator to override AI's pile when it disagrees with its
// own score.
func VerdictFromScore(overall int) VerdictPile {
	switch {
	case overall >= 85:
		return VerdictYes
	case overall >= 60:
		return VerdictMaybe
	default:
		return VerdictNo
	}
}
