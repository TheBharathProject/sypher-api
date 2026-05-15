package billing

// Costs is the authoritative per-operation credit pricing for paid AI
// features. The frontend mirrors these constants in lib/billing.ts so
// it can render "this will cost N credits" without an extra round-trip,
// but the server is the source of truth — every Spend call must use a
// value from this file.
//
// Gating model (see jobtracker handlers):
//   1. Try the free monthly token quota first (AI_USAGE_MONTHLY_TOKEN_LIMIT,
//      default 25k tokens, EnforceLimit in internal/ai/usage.go).
//   2. If exhausted, debit the paid credits balance by Costs[reason].
//   3. If the user has neither, return 402 insufficient_credits.
//
// Each cost is denominated in "credits" — the unit users buy via the
// credit packs (see packs.go). One credit ≈ ₹1.50 at the smallest pack;
// blends cheaper on larger packs.
const (
	// CostResumeReport — full resume analysis with score, strengths,
	// gaps, action items. Most token-intensive AI call we run.
	CostResumeReport = 25

	// CostResumeTweak — modifies an existing resume to better match a
	// cover-letter or job-description prompt. Returns markdown.
	CostResumeTweak = 20

	// CostCoverLetter — generates a tailored cover letter for a given
	// job description + resume.
	CostCoverLetter = 10

	// CostResumeParse — converts a candidate's existing resume (PDF
	// text or pasted text) into structured DraftContent for the Resume
	// Builder. Cheaper than ResumeReport because the prompt is a
	// straight structural extraction, not an analytical review.
	CostResumeParse = 10
)

// AICostReason is the canonical identifier for each spend reason. Using
// constants here means the ledger's `reason` column has a small,
// queryable vocabulary instead of free-form strings.
const (
	ReasonResumeReport = "ai_resume_report"
	ReasonResumeTweak  = "ai_resume_tweak"
	ReasonCoverLetter  = "ai_cover_letter"
	ReasonResumeParse  = "ai_resume_parse"
)
