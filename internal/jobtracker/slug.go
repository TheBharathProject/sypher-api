package jobtracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// consecutiveHyphens matches two or more consecutive hyphens, collapsing them
// into a single hyphen after whitespace has already been converted to hyphens.
// Compiled once at package init; slugify must not compile it on each call.
var consecutiveHyphens = regexp.MustCompile(`-{2,}`)

// nonSlugChar matches any character that is not a letter, digit, space, or hyphen.
// These are stripped in step 2 of slugify.
var nonSlugChar = regexp.MustCompile(`[^a-z0-9 \-]`)

// slugify converts a post title into a URL-safe slug. Pure function — no I/O.
//
// Algorithm (6 steps):
//  1. Lowercase the title.
//  2. Strip any character that is not a-z, 0-9, space, or hyphen.
//  3. Trim leading/trailing whitespace.
//  4. Collapse runs of whitespace/hyphens into a single hyphen.
//  5. Truncate to 80 chars; back up to the last hyphen boundary if one exists
//     past position 40 to avoid cutting mid-word.
//  6. If the result is empty, return "" — the caller (uniqueSlug) substitutes "post".
func slugify(title string) string {
	// 1. Lowercase.
	s := strings.ToLower(title)

	// 2. Strip non-slug characters (anything that isn't a-z, 0-9, space, or hyphen).
	s = nonSlugChar.ReplaceAllString(s, "")

	// 3. Trim whitespace.
	s = strings.TrimSpace(s)

	// 4. Replace all whitespace with hyphens, then collapse consecutive hyphens.
	s = strings.ReplaceAll(s, " ", "-")
	s = consecutiveHyphens.ReplaceAllString(s, "-")

	// 5. Truncate to 80 chars, trimming at a hyphen boundary when possible.
	if len(s) > 80 {
		s = s[:80]
		if idx := strings.LastIndexByte(s, '-'); idx > 40 {
			s = s[:idx]
		}
		// Remove any trailing hyphen left by a hard cut at exactly 80.
		s = strings.TrimRight(s, "-")
	}

	// 6. Empty result — uniqueSlug handles the "post" fallback.
	return s
}

// uniqueSlug finds a slug that is not already taken in job_tracker.community_posts.
// It tries the base slug up to 5 times, appending a random 4-char hex suffix on
// each subsequent attempt (2 crypto/rand bytes → 4 hex chars).
//
// Callers must treat a non-nil error as a signal to abort the insert.
func (s *Store) uniqueSlug(ctx context.Context, title string) (string, error) {
	base := slugify(title)
	if base == "" {
		base = "post"
	}

	candidate := base
	for attempt := 0; attempt < 5; attempt++ {
		var exists bool
		err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM job_tracker.community_posts WHERE slug = $1)`,
			candidate,
		).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}

		// Generate a 4-char hex suffix from 2 crypto/rand bytes.
		b := make([]byte, 2)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		// Ensure the base we suffix doesn't exceed 80 chars when the suffix is appended.
		trimmed := base
		if len(trimmed) > 80 {
			trimmed = trimmed[:80]
		}
		candidate = trimmed + "-" + hex.EncodeToString(b)
	}

	return "", fmt.Errorf("slug: exhausted retries for %q", base)
}
