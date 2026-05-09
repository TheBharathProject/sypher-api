package importers

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// linkedInImporter reads LinkedIn's "Job Application Insights" CSV from
// the My Network → Settings & Privacy → Get a copy of your data export.
//
// Column names from LinkedIn's tool have shifted at least twice in the
// last year (Date / Application Date / Submitted Date are all variants).
// We accept synonyms per output field — see ADR-004 D3 — so a single
// rename doesn't take down the import flow.
type linkedInImporter struct{}

func (linkedInImporter) Name() string { return "linkedin" }

func (linkedInImporter) Parse(r io.Reader) ([]ApplicationInput, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("empty CSV")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	idx := indexHeader(header)

	// Required: company + role (LinkedIn always exports both — bail if
	// the export is missing them, that's a different file).
	companyCol := firstMatch(idx, "Company Name", "Company", "Employer")
	titleCol := firstMatch(idx, "Job Title", "Title", "Position", "Role")
	if companyCol < 0 || titleCol < 0 {
		return nil, nil, fmt.Errorf("LinkedIn CSV missing Company Name or Job Title columns")
	}

	// Optional, mapped via firstMatch so renames don't break us.
	dateCol := firstMatch(idx, "Application Date", "Submitted Date", "Date Applied", "Date")
	urlCol := firstMatch(idx, "Job Url", "Job URL", "Url", "Posting Url")
	statusCol := firstMatch(idx, "Status", "Application Status")
	locationCol := firstMatch(idx, "Location", "Job Location")

	rows := []ApplicationInput{}
	errs := []string{}

	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("line %d: %s", line, err.Error()))
			continue
		}
		col := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return trim(rec[i])
		}

		in := ApplicationInput{
			Company:  col(companyCol),
			Role:     col(titleCol),
			Source:   "LinkedIn",
			Location: col(locationCol),
			JobLink:  col(urlCol),
			Stage:    mapLinkedInStatus(col(statusCol)),
		}

		if d := col(dateCol); d != "" {
			if iso, ok := normalizeDate(d); ok {
				in.AppliedAt = iso
			} else {
				errs = append(errs, fmt.Sprintf("line %d: unrecognised date %q (skipped)", line, d))
			}
		}

		if in.Company == "" || in.Role == "" {
			errs = append(errs, fmt.Sprintf("line %d: company and role are required", line))
			continue
		}
		rows = append(rows, in)
	}
	return rows, errs, nil
}

// mapLinkedInStatus turns LinkedIn's status strings into Pegasus stage
// values. LinkedIn exports things like "Submitted", "Application
// Viewed", "Interviewing", "Not Selected". We collapse to the closest
// Pegasus stage; unknown strings default to APPLIED (not INTERESTED —
// LinkedIn only tracks rows the user actually applied to).
func mapLinkedInStatus(s string) string {
	switch strings.TrimSpace(s) {
	case "":
		return "APPLIED"
	case "Submitted", "Application Viewed", "Resume Downloaded":
		return "APPLIED"
	case "Interviewing", "Interview Scheduled":
		return "TECHNICAL"
	case "Phone Screen":
		return "PHONE_SCREEN"
	case "Onsite", "On-site":
		return "ONSITE"
	case "Offer", "Offer Extended":
		return "OFFER"
	case "Not Selected", "Rejected", "Withdrawn":
		return "REJECTED"
	default:
		return "APPLIED"
	}
}

func trim(s string) string { return strings.TrimSpace(s) }
