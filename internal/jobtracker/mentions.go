package jobtracker

import "regexp"

// Mention regex: `@<slug>` where slug obeys the same rules as profile
// slugs (lowercase a-z, 0-9, dash; 3-40 chars). Word boundary at the
// end so `@bob,` matches `bob`. Matches must START with a letter or
// digit (not a dash) to avoid `@-bob` weirdness.
//
// Anchored to a non-letter prefix or start-of-string so we don't
// accidentally capture mid-word @s like email addresses (`hi@example.com`
// must not match `example`). Implemented via a negative-lookbehind-
// equivalent: we look at the preceding rune in app code rather than the
// regex, which Go's RE2 doesn't support for lookbehind.
var mentionRe = regexp.MustCompile(`@([a-z0-9][a-z0-9-]{2,39})`)

// ExtractMentions returns the set of unique slug strings mentioned in
// the body. Excludes mentions that appear to be inside email addresses
// (preceding character is a letter / digit). Output is deterministic-
// ordered (insertion order, deduped) so notifier writes are stable.
func ExtractMentions(body string) []string {
	if body == "" {
		return nil
	}
	indices := mentionRe.FindAllStringSubmatchIndex(body, -1)
	if len(indices) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, m := range indices {
		// m[0] is the start of the @ symbol. If the preceding character
		// is a letter or digit, this is an email-style mid-word @ —
		// skip it.
		start := m[0]
		if start > 0 {
			prev := body[start-1]
			if isAlnum(prev) {
				continue
			}
		}
		slug := body[m[2]:m[3]]
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
