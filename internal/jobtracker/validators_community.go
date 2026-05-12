package jobtracker

import "encoding/json"

// metaValidationError carries the field name and human-readable message for
// a failed per-surface metadata validation. The handler serialises both into
// the three-key error response shape.
type metaValidationError struct {
	Field   string
	Message string
}

func (e *metaValidationError) Error() string { return e.Field + ": " + e.Message }

// validateCommunityMetadata validates the per-surface metadata JSON attached
// to a community post. Returns nil when the metadata is valid or absent.
// Returns a *metaValidationError describing the first violation found.
//
// Unknown surfaces and nil/empty raw messages are accepted without error —
// this function only blocks values that are present and provably wrong.
func validateCommunityMetadata(surface string, raw json.RawMessage) *metaValidationError {
	if len(raw) == 0 {
		return nil
	}
	switch surface {
	case "experiences":
		return validateExperiencesMeta(raw)
	case "ask":
		return validateAskMeta(raw)
	case "recruiters":
		return validateRecruiterMeta(raw)
	case "reviews":
		return validateReviewsMeta(raw)
	case "referrals":
		return nil
	default:
		return nil
	}
}

// validateExperiencesMeta validates outcome and difficulty fields.
func validateExperiencesMeta(raw json.RawMessage) *metaValidationError {
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return &metaValidationError{Field: "metadata", Message: "invalid JSON"}
	}

	validOutcomes := map[string]bool{
		"Offer":      true,
		"Reject":     true,
		"Ghosted":    true,
		"InProgress": true,
		"Withdrew":   true,
	}
	if outcome, ok := meta["outcome"].(string); ok && outcome != "" {
		if !validOutcomes[outcome] {
			return &metaValidationError{Field: "outcome", Message: "must be one of Offer, Reject, Ghosted, InProgress, Withdrew"}
		}
	}

	validDifficulty := map[string]bool{
		"Easy":   true,
		"Medium": true,
		"Hard":   true,
	}
	if difficulty, ok := meta["difficulty"].(string); ok && difficulty != "" {
		if !validDifficulty[difficulty] {
			return &metaValidationError{Field: "difficulty", Message: "must be one of Easy, Medium, Hard"}
		}
	}

	return nil
}

// validateAskMeta validates the tags array: at most 3 entries, each from the
// canonical allowlist.
func validateAskMeta(raw json.RawMessage) *metaValidationError {
	var meta struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return &metaValidationError{Field: "metadata", Message: "invalid JSON"}
	}

	if len(meta.Tags) > 3 {
		return &metaValidationError{Field: "tags", Message: "at most 3 tags allowed"}
	}

	validTags := map[string]bool{
		"Career":       true,
		"Interview":    true,
		"Compensation": true,
		"Remote":       true,
		"Visa":         true,
		"Internship":   true,
		"Fresher":      true,
		"Layoffs":      true,
		"Tools":        true,
		"Other":        true,
	}
	for _, tag := range meta.Tags {
		if !validTags[tag] {
			return &metaValidationError{Field: "tags", Message: "tag \"" + tag + "\" is not in the allowed list"}
		}
	}

	return nil
}

// validateRecruiterMeta validates specializations and hiringLevels arrays.
func validateRecruiterMeta(raw json.RawMessage) *metaValidationError {
	var meta struct {
		Specializations []string `json:"specializations"`
		HiringLevels    []string `json:"hiringLevels"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return &metaValidationError{Field: "metadata", Message: "invalid JSON"}
	}

	validSpecializations := map[string]bool{
		"Tech":      true,
		"Non-Tech":  true,
		"Executive": true,
		"Campus":    true,
		"Contract":  true,
		"Other":     true,
	}
	for _, s := range meta.Specializations {
		if !validSpecializations[s] {
			return &metaValidationError{Field: "specializations", Message: "\"" + s + "\" is not a valid specialization"}
		}
	}

	validHiringLevels := map[string]bool{
		"Fresher":   true,
		"Junior":    true,
		"Mid":       true,
		"Senior":    true,
		"Lead":      true,
		"Manager":   true,
		"Director":  true,
		"Executive": true,
	}
	for _, l := range meta.HiringLevels {
		if !validHiringLevels[l] {
			return &metaValidationError{Field: "hiringLevels", Message: "\"" + l + "\" is not a valid hiring level"}
		}
	}

	return nil
}

// validateReviewsMeta validates targetRole and experienceLevel fields.
func validateReviewsMeta(raw json.RawMessage) *metaValidationError {
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return &metaValidationError{Field: "metadata", Message: "invalid JSON"}
	}

	validTargetRoles := map[string]bool{
		"SDE":          true,
		"PM":           true,
		"Data Science": true,
		"Design":       true,
		"DevOps":       true,
		"QA":           true,
		"Other":        true,
	}
	if role, ok := meta["targetRole"].(string); ok && role != "" {
		if !validTargetRoles[role] {
			return &metaValidationError{Field: "targetRole", Message: "\"" + role + "\" is not a valid target role"}
		}
	}

	validExperienceLevels := map[string]bool{
		"Fresher":       true,
		"Junior (0-2)":  true,
		"Mid (2-5)":     true,
		"Senior (5+)":   true,
		"Lead (8+)":     true,
	}
	if level, ok := meta["experienceLevel"].(string); ok && level != "" {
		if !validExperienceLevels[level] {
			return &metaValidationError{Field: "experienceLevel", Message: "\"" + level + "\" is not a valid experience level"}
		}
	}

	return nil
}
