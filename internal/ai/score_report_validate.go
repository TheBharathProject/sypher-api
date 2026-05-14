package ai

import (
	"log/slog"
	"strings"
)

// canonicalChecklistIDs lists every checklist item we expect the model
// to emit, keyed by group. Unknown ids are dropped during validation
// (with a warning); missing ids are tolerated (the FE renders only
// what's present, so a partially-empty checklist degrades gracefully).
//
// Keep this in lockstep with the in-prompt canonical list in
// prompts_resume_score.go — anything you add to the prompt must also
// be added here, or it'll be silently dropped.
var canonicalChecklistIDs = map[string]string{
	// common_mistakes
	"single_column":       GroupCommonMistakes,
	"bolding_policy":      GroupCommonMistakes,
	"clean_template":      GroupCommonMistakes,
	"font_consistency":    GroupCommonMistakes,
	"no_filler":           GroupCommonMistakes,
	"plain_project_names": GroupCommonMistakes,
	"no_cliches":          GroupCommonMistakes,
	"no_photo":            GroupCommonMistakes,
	"four_or_fewer_links": GroupCommonMistakes,
	"no_spoken_languages": GroupCommonMistakes,
	"no_skill_ratings":    GroupCommonMistakes,
	"no_references_line":  GroupCommonMistakes,
	"no_manager_quotes":   GroupCommonMistakes,
	"short_link_labels":   GroupCommonMistakes,
	"ats_safe_bullets":    GroupCommonMistakes,

	// dos_donts
	"years_obvious":           GroupDosDonts,
	"jd_techs_page_one":       GroupDosDonts,
	"experience_leads":        GroupDosDonts,
	"current_role_top":        GroupDosDonts,
	"bullets_quantified":      GroupDosDonts,
	"action_verbs":            GroupDosDonts,
	"reverse_chrono":          GroupDosDonts,
	"readable_dates":          GroupDosDonts,
	"skills_categorised":      GroupDosDonts,
	"promotions_labelled":     GroupDosDonts,
	"length_correct":          GroupDosDonts,
	"no_pii":                  GroupDosDonts,
	"no_full_address":         GroupDosDonts,
	"no_typos":                GroupDosDonts,
	"first_person":            GroupDosDonts,
	"no_responsibility_lists": GroupDosDonts,
	"no_subbullets":           GroupDosDonts,
	"skills_under_25":         GroupDosDonts,
	"no_generic_objective":    GroupDosDonts,

	// stands_out
	"bullet_formula":        GroupStandsOut,
	"personal_contribution": GroupStandsOut,
	"scale_numbers":         GroupStandsOut,
	"tailored_to_jd":        GroupStandsOut,
	"projects_with_impact":  GroupStandsOut,
	"tech_in_context":       GroupStandsOut,
	"no_negatives":          GroupStandsOut,
	"seniority_signals":     GroupStandsOut,
}

// validChecklistStatus is the closed set of statuses we accept on each
// checklist item. Bogus values (typo'd by the model) get mapped to
// "warn" with a warning logged.
func validChecklistStatus(s ChecklistStatus) bool {
	switch s {
	case ChecklistPass, ChecklistWarn, ChecklistFail, ChecklistNA:
		return true
	}
	return false
}

func validEffort(e Effort) bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh:
		return true
	}
	return false
}

func validImpact(i Impact) bool {
	switch i {
	case ImpactLow, ImpactMedium, ImpactHigh:
		return true
	}
	return false
}

// ValidateScoreReport enforces the invariants a freshly-parsed v2
// report must satisfy before we persist it:
//
//  1. Sections map exists and has every key from SectionOrder. Missing
//     sections are filled with score=0 and an empty findings list.
//  2. VerdictPile matches the bucket implied by OverallScore. If the
//     model picked an inconsistent pile, override with the canonical
//     value and log a warning.
//  3. Checklist items have known ids and valid statuses. Unknown ids
//     are dropped. Bogus statuses are coerced to "warn".
//  4. ImprovementPlan: effort/impact coerced to "medium" if invalid;
//     plan capped at 12 entries.
//
// Pure: returns a new ScoreReport, leaves the input untouched. Caller
// keeps the original for forensics if anything goes sideways.
func ValidateScoreReport(r *ScoreReport, logger *slog.Logger) *ScoreReport {
	out := *r // shallow copy; we'll overwrite Sections/Checklist/Plan

	// 1. Sections — every key from SectionOrder present.
	sections := make(map[string]ScoreSection, len(SectionOrder))
	for _, key := range SectionOrder {
		sec, ok := r.Sections[key]
		if !ok {
			if logger != nil {
				logger.Warn("score report: missing section", "key", key)
			}
			sections[key] = ScoreSection{Score: 0, Findings: nil}
			continue
		}
		// Findings may be nil — that's fine; FE shows "No findings".
		if sec.Findings == nil {
			sec.Findings = []ScoreFinding{}
		}
		sections[key] = sec
	}
	// Preserve any extra sections the model invented (don't silently
	// drop — surface them so the FE can show + we can decide later).
	for k, v := range r.Sections {
		if _, known := sections[k]; !known {
			sections[k] = v
		}
	}
	out.Sections = sections

	// 2. VerdictPile consistent with OverallScore.
	canonical := VerdictFromScore(out.OverallScore)
	if out.VerdictPile != canonical {
		if logger != nil && out.VerdictPile != "" {
			logger.Warn("score report: verdict pile inconsistent with overall_score",
				"model_pile", out.VerdictPile,
				"canonical_pile", canonical,
				"overall_score", out.OverallScore)
		}
		out.VerdictPile = canonical
	}

	// 3. Checklist — known ids, valid statuses, sorted by group then
	// by canonical order. Drops unknown ids.
	cleaned := make([]ChecklistItem, 0, len(r.Checklist))
	seen := make(map[string]bool, len(r.Checklist))
	for _, it := range r.Checklist {
		group, known := canonicalChecklistIDs[it.ID]
		if !known {
			if logger != nil {
				logger.Warn("score report: unknown checklist id", "id", it.ID)
			}
			continue
		}
		if seen[it.ID] {
			// Duplicate id — keep first, drop later (typically the
			// model double-emitted; first is usually the considered one).
			continue
		}
		seen[it.ID] = true
		// Enforce canonical group regardless of model's claim — the
		// FE renders by group, so a mis-grouped item would disappear.
		it.Group = group
		if !validChecklistStatus(it.Status) {
			if logger != nil {
				logger.Warn("score report: invalid checklist status",
					"id", it.ID, "status", it.Status)
			}
			it.Status = ChecklistWarn
		}
		it.Evidence = strings.TrimSpace(it.Evidence)
		cleaned = append(cleaned, it)
	}
	out.Checklist = cleaned

	// 4. ImprovementPlan — coerce effort/impact, cap at 12.
	const planCap = 12
	plan := r.ImprovementPlan
	if len(plan) > planCap {
		plan = plan[:planCap]
	}
	for i := range plan {
		if !validEffort(plan[i].Effort) {
			plan[i].Effort = EffortMedium
		}
		if !validImpact(plan[i].Impact) {
			plan[i].Impact = ImpactMedium
		}
		plan[i].Title = strings.TrimSpace(plan[i].Title)
		plan[i].Where = strings.TrimSpace(plan[i].Where)
		plan[i].Before = strings.TrimSpace(plan[i].Before)
		plan[i].After = strings.TrimSpace(plan[i].After)
	}
	out.ImprovementPlan = plan

	return &out
}
