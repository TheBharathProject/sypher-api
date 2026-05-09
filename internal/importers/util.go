package importers

import (
	"strings"
	"time"
)

// indexHeader normalises a CSV header row to a lookup map of canonical
// names → column index. Lowercases, trims, strips spaces — so
// "Company Name" and "company_name" and "company name " all collide
// to "companyname". Importers map their canonical-name list against
// this; see firstMatch().
func indexHeader(row []string) map[string]int {
	out := make(map[string]int, len(row))
	for i, h := range row {
		out[normalizeKey(h)] = i
	}
	return out
}

// normalizeKey: lowercase + remove spaces and underscores. Friendly to
// "Job Title" vs "job_title" vs "jobTitle" without per-importer fuss.
func normalizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r == ' ' || r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// firstMatch returns the column index for the first variant that exists
// in the header. Used by the LinkedIn / Naukri importers which have a
// list of accepted aliases per output field. -1 if none match.
func firstMatch(idx map[string]int, variants ...string) int {
	for _, v := range variants {
		if i, ok := idx[normalizeKey(v)]; ok {
			return i
		}
	}
	return -1
}

// getter binds a record to the indexed header so callers can do
// get("company") instead of plumbing rec/idx through every line.
func getter(idx map[string]int, rec []string) func(string) string {
	return func(canonical string) string {
		i, ok := idx[normalizeKey(canonical)]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// dateLayouts is the list of date formats we recognise across all
// import sources. Each importer may scope to a subset, but having one
// list here means a CSV that mixes formats (rare but possible) still
// parses cleanly.
//
// All output dates are normalised to YYYY-MM-DD (the shape the store
// expects).
var dateLayouts = []string{
	"2006-01-02",       // ISO — Naukri, our own template
	"01/02/2006",       // US — LinkedIn
	"02/01/2006",       // EU
	"2006/01/02",       // ISO with slashes
	"Jan 2, 2006",      // long
	"January 2, 2006",  // longer
	"02-Jan-2006",      // short month
	"2006-01-02T15:04:05Z", // RFC3339 (LinkedIn export sometimes embeds)
}

// normalizeDate tries each layout in order, returns the YYYY-MM-DD form
// or empty string + false if nothing parses. Importers call this on
// date columns and surface a per-row warning when it returns false so
// the user can spot-check.
func normalizeDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true // empty is fine, not a parse failure
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	return "", false
}
