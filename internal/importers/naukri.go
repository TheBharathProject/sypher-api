package importers

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// naukriImporter reads a Naukri job-application export. The export
// format is less stable than LinkedIn's; common columns we've seen:
//
//	Company, Designation, Job Location, Application Date,
//	Application Status, Salary, CTC, Notice Period
//
// Required: Company + Designation (the role). Everything else best-
// effort. Per ADR-004 D3 we accept synonyms for resilience.
type naukriImporter struct{}

func (naukriImporter) Name() string { return "naukri" }

func (naukriImporter) Parse(r io.Reader) ([]ApplicationInput, []string, error) {
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

	companyCol := firstMatch(idx, "Company", "Company Name", "Employer")
	roleCol := firstMatch(idx, "Designation", "Job Title", "Role", "Position")
	if companyCol < 0 || roleCol < 0 {
		return nil, nil, fmt.Errorf("Naukri CSV missing Company or Designation columns")
	}

	dateCol := firstMatch(idx, "Application Date", "Applied On", "Date Applied", "Date")
	urlCol := firstMatch(idx, "Job URL", "Job Link", "URL")
	statusCol := firstMatch(idx, "Application Status", "Status")
	locationCol := firstMatch(idx, "Job Location", "Location")
	salaryCol := firstMatch(idx, "Salary", "CTC", "Compensation")

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
			return strings.TrimSpace(rec[i])
		}

		in := ApplicationInput{
			Company:     col(companyCol),
			Role:        col(roleCol),
			Source:      "Naukri",
			Location:    col(locationCol),
			JobLink:     col(urlCol),
			SalaryRange: col(salaryCol),
			Stage:       mapNaukriStatus(col(statusCol)),
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

// mapNaukriStatus mirrors mapLinkedInStatus but for Naukri's status
// strings. Naukri uses "Auto Applied", "Application Viewed",
// "Shortlisted", "Rejected" — closer to LinkedIn semantically but the
// strings differ.
func mapNaukriStatus(s string) string {
	switch strings.TrimSpace(s) {
	case "":
		return "APPLIED"
	case "Auto Applied", "Applied", "Application Sent":
		return "APPLIED"
	case "Application Viewed", "Profile Viewed":
		return "APPLIED"
	case "Shortlisted":
		return "PHONE_SCREEN"
	case "Interview Scheduled", "Interview":
		return "TECHNICAL"
	case "Offer":
		return "OFFER"
	case "Rejected", "Not Shortlisted":
		return "REJECTED"
	default:
		return "APPLIED"
	}
}
