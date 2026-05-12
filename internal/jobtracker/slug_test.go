package jobtracker

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal ASCII title",
			input: "My Google L5 Interview Went!",
			want:  "my-google-l5-interview-went",
		},
		{
			name:  "title with hyphenated word",
			input: "How to crack SDE-2 at Amazon",
			want:  "how-to-crack-sde-2-at-amazon",
		},
		{
			name:  "only special chars — returns empty (uniqueSlug handles fallback)",
			input: "!!!",
			want:  "",
		},
		{
			name:  "leading and trailing spaces trimmed",
			input: "  leading spaces  ",
			want:  "leading-spaces",
		},
		{
			name:  "consecutive hyphens collapsed",
			input: "a--b---c",
			want:  "a-b-c",
		},
		{
			name:  "title longer than 80 chars truncated at 80",
			input: strings.Repeat("x", 100),
			want:  strings.Repeat("x", 80),
		},
		{
			name:  "Unicode/Hindi title drops non-ASCII, keeps ASCII words",
			input: "गूगल interview experience",
			want:  "interview-experience",
		},
		{
			name:  "empty string returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "only non-ASCII chars returns empty",
			input: "गूगल",
			want:  "",
		},
		{
			name:  "mixed spaces and hyphens collapsed",
			input: "hello -  world",
			want:  "hello-world",
		},
		{
			name:  "title exactly 80 chars not truncated",
			input: strings.Repeat("a", 80),
			want:  strings.Repeat("a", 80),
		},
		{
			name:  "title with punctuation stripped correctly",
			input: "What's the best way to prepare? (2024)",
			want:  "whats-the-best-way-to-prepare-2024",
		},
		{
			name:  "title over 80 chars with hyphen past 40 — truncated at hyphen boundary",
			// Build: 50 'a's + '-' + 40 'b's = 91 chars.
			// After lowercase + truncate to 80: "aaa...aaa-bbb...bbb" (50 a's + '-' + 29 b's).
			// Last hyphen at index 50, which is > 40, so we truncate there → 50 a's.
			input: strings.Repeat("a", 50) + "-" + strings.Repeat("b", 40),
			want:  strings.Repeat("a", 50),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
