package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParsedResumePersonal mirrors jobtracker.DraftPersonal so the AI
// client doesn't have to import the jobtracker package. The handler
// converts ParsedResume → DraftContent.
type ParsedResumePersonal struct {
	Name        string `json:"name,omitempty"`
	Headline    string `json:"headline,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Location    string `json:"location,omitempty"`
	LinkedinURL string `json:"linkedinUrl,omitempty"`
	GithubURL   string `json:"githubUrl,omitempty"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
}

type ParsedResumeExperience struct {
	Company     string   `json:"company,omitempty"`
	Title       string   `json:"title,omitempty"`
	Location    string   `json:"location,omitempty"`
	StartDate   string   `json:"startDate,omitempty"`
	EndDate     string   `json:"endDate,omitempty"`
	Current     bool     `json:"current,omitempty"`
	Description []string `json:"description,omitempty"`
}

type ParsedResumeEducation struct {
	School      string `json:"school,omitempty"`
	Degree      string `json:"degree,omitempty"`
	Field       string `json:"field,omitempty"`
	StartDate   string `json:"startDate,omitempty"`
	EndDate     string `json:"endDate,omitempty"`
	GPA         string `json:"gpa,omitempty"`
	Description string `json:"description,omitempty"`
}

type ParsedResumeProject struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	TechStack   string `json:"techStack,omitempty"`
	Link        string `json:"link,omitempty"`
}

type ParsedResumeSkillGroup struct {
	Category string   `json:"category,omitempty"`
	Items    []string `json:"items,omitempty"`
}

type ParsedResume struct {
	Personal    ParsedResumePersonal     `json:"personal"`
	Summary     string                   `json:"summary,omitempty"`
	Experiences []ParsedResumeExperience `json:"experiences,omitempty"`
	Educations  []ParsedResumeEducation  `json:"educations,omitempty"`
	Projects    []ParsedResumeProject    `json:"projects,omitempty"`
	Skills      []ParsedResumeSkillGroup `json:"skills,omitempty"`
}

// ResumeParseToContent extracts structured resume fields from plain
// text. Temperature is intentionally low (0.2) — we want a deterministic
// structural extraction, not creative rewriting. Retries once if the
// model returns malformed JSON, with the parse error inlined into the
// retry prompt — mirrors the score-report pattern.
func (c *Client) ResumeParseToContent(ctx context.Context, resumeText string) (*ParsedResume, *CompletionResult, error) {
	if strings.TrimSpace(resumeText) == "" {
		return nil, nil, errors.New("empty resume text")
	}

	res, err := c.complete(ctx, resumeParseSystemPrompt, resumeText, 0.2)
	if err != nil {
		return nil, nil, err
	}

	parsed, parseErr := parseResumeJSON(res.Text)
	if parseErr == nil {
		return parsed, res, nil
	}

	retryUser := resumeText + "\n\n" + fmt.Sprintf(resumeParseRetryPrompt, parseErr.Error())
	res2, err := c.complete(ctx, resumeParseSystemPrompt, retryUser, 0.1)
	if err != nil {
		return nil, nil, fmt.Errorf("resume parse retry: %w", err)
	}
	parsed2, parseErr2 := parseResumeJSON(res2.Text)
	if parseErr2 != nil {
		return nil, res2, fmt.Errorf("resume parse: malformed JSON after retry: %w", parseErr2)
	}
	res2.TokensIn += res.TokensIn
	res2.TokensOut += res.TokensOut
	return parsed2, res2, nil
}

// parseResumeJSON tolerates common model quirks: markdown fences, a
// trailing "thought" tail, leading whitespace.
func parseResumeJSON(raw string) (*ParsedResume, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	if start := strings.Index(s, "{"); start > 0 {
		s = s[start:]
	}
	if end := strings.LastIndex(s, "}"); end >= 0 && end < len(s)-1 {
		s = s[:end+1]
	}
	var p ParsedResume
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
