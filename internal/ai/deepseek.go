// Package ai wraps the Deepseek (OpenAI-compatible) chat-completions API.
// Deepseek's contract is a near-clone of OpenAI's, so we hit it with plain
// net/http rather than pulling in an SDK.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to Deepseek's chat-completions endpoint.
type Client struct {
	apiKey  string
	baseURL string
	model   string
	httpc   *http.Client
}

// Settings is the subset of config the AI client cares about.
type Settings struct {
	APIKey  string
	BaseURL string // e.g. https://api.deepseek.com/v1
	Model   string // e.g. deepseek-chat
}

// New constructs a Client. Returns an error if the API key is missing.
func New(s Settings) (*Client, error) {
	if s.APIKey == "" {
		return nil, errors.New("ai: missing DEEPSEEK_API_KEY")
	}
	if s.BaseURL == "" {
		s.BaseURL = "https://api.deepseek.com/v1"
	}
	if s.Model == "" {
		s.Model = "deepseek-chat"
	}
	return &Client{
		apiKey:  s.APIKey,
		baseURL: strings.TrimRight(s.BaseURL, "/"),
		model:   s.Model,
		httpc:   &http.Client{Timeout: 90 * time.Second},
	}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// CompletionResult bundles the LLM output with usage stats so callers can
// record cost without doing a second round-trip.
type CompletionResult struct {
	Text      string
	TokensIn  int
	TokensOut int
}

func (c *Client) complete(ctx context.Context, system, user string, temp float64) (*CompletionResult, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: temp,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("deepseek status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode deepseek response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("deepseek returned no choices")
	}
	return &CompletionResult{
		Text:      parsed.Choices[0].Message.Content,
		TokensIn:  parsed.Usage.PromptTokens,
		TokensOut: parsed.Usage.CompletionTokens,
	}, nil
}

// ResumeReport produces a structured Markdown review of the given resume text.
func (c *Client) ResumeReport(ctx context.Context, resumeText string) (*CompletionResult, error) {
	const system = `You are a senior career coach. Review the candidate's resume and return Markdown with these sections in order:

## Summary
A 2-3 sentence high-level read.

## Strengths
- bullet list

## Gaps
- bullet list

## Recommended edits
- concrete, line-level suggestions where possible

## Score
Give a single integer score from 0 to 100 inside backticks like ` + "`82`" + ` (this exact format - one line, just the number in backticks).

Be specific and actionable. Avoid generic advice.`
	return c.complete(ctx, system, resumeText, 0.4)
}

// CoverLetter writes a tailored cover letter from a JD + resume snippet.
func (c *Client) CoverLetter(ctx context.Context, jobDescription, resumeText string) (*CompletionResult, error) {
	const system = `You write tight, specific cover letters. Produce 3-4 short paragraphs in plain prose.
- Open with a single specific reason this candidate fits THIS role (no platitudes).
- Cite 1-2 concrete experiences from the resume.
- Close with a clear ask for a conversation.
- No filler, no exaggerated claims, no adjectives like "passionate" or "innovative".
Output the letter only, no preamble.`
	user := fmt.Sprintf("Job description:\n%s\n\n---\n\nCandidate resume:\n%s", jobDescription, resumeText)
	return c.complete(ctx, system, user, 0.6)
}

// ResumeTweak rewrites the candidate's resume so it lands harder for a
// specific JD or cover-letter context. Returns the full rewritten resume
// in Markdown — the user (and the FE) can diff against source_text to
// see what changed.
//
// Style discipline matches CoverLetter: no platitudes, no inflated
// adjectives, factual edits only.
func (c *Client) ResumeTweak(ctx context.Context, sourceResume, prompt string) (*CompletionResult, error) {
	const system = `You are a senior career coach editing a candidate's resume for a specific role.

Rules:
- Preserve every factual claim from the original resume. Do NOT invent
  jobs, dates, metrics, or technologies. Only edit phrasing, ordering,
  emphasis, and section structure.
- Re-rank the candidate's bullets so the most relevant points for THIS
  role appear first within each section.
- Tighten language: prefer concrete numbers and verbs; drop adjectives
  like "passionate", "innovative", "robust".
- Match keywords from the prompt (JD or cover letter) where the original
  resume already supports them. Don't keyword-stuff.
- Keep the structure as familiar resume sections: Summary, Experience,
  Projects, Education, Skills. Use Markdown headings.

Return ONLY the rewritten resume in Markdown. No preamble, no explanation.`
	user := fmt.Sprintf("Context (job description or cover letter):\n%s\n\n---\n\nOriginal resume:\n%s", prompt, sourceResume)
	return c.complete(ctx, system, user, 0.4)
}
