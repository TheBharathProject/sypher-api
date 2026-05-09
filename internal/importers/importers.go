// Package importers parses bulk-import payloads from various external
// sources into the canonical ApplicationInput shape that the jobtracker
// store expects.
//
// One Importer impl per source. New sources plug in via a single switch
// case in For() — no conditional logic in the calling handler. See
// docs/adr/0004-bulk-import-sources.md for the design.
package importers

import (
	"fmt"
	"io"
	"strings"
)

// ApplicationInput mirrors jobtracker.ApplicationInput. Duplicated here
// to avoid a cyclic import (importers is consumed by jobtracker handlers,
// so it can't import jobtracker types). The caller maps these to the
// real ApplicationInput one-for-one.
type ApplicationInput struct {
	Company        string
	Role           string
	Source         string
	Location       string
	SalaryRange    string
	Stage          string
	AppliedAt      string // YYYY-MM-DD or empty
	ApplyDeadline  string // YYYY-MM-DD or empty
	JobLink        string
	JobDescription string
	Notes          string
	Stale          bool
}

// Importer turns a stream of bytes into structured application rows.
// Implementations:
//   - csvImporter: native Pegasus CSV (the format ApplicationsTemplate emits)
//   - linkedInImporter: LinkedIn "My Jobs" job-application export CSV
//   - naukriImporter: Naukri export CSV
//
// `errs` is a per-row issue list (bad date format, missing optional
// column for one row, etc.) — non-fatal. `err` is parser-level fatal
// (unreadable CSV, missing required column).
type Importer interface {
	Name() string
	Parse(r io.Reader) (rows []ApplicationInput, errs []string, err error)
}

// For returns the importer for a given source key. Unknown source → error.
// Empty string defaults to "csv" so legacy callers without a source param
// keep working. See ADR-004 D2.
func For(source string) (Importer, error) {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "csv":
		return &csvImporter{}, nil
	case "linkedin", "linkedin_csv":
		return &linkedInImporter{}, nil
	case "naukri", "naukri_csv":
		return &naukriImporter{}, nil
	default:
		return nil, fmt.Errorf("unknown import source: %q (expected csv|linkedin|naukri)", source)
	}
}

// AvailableSources returns the source strings the frontend should offer.
// Useful as a tiny stable contract — saves the frontend from hardcoding
// the same list a second time.
func AvailableSources() []string {
	return []string{"csv", "linkedin", "naukri"}
}
