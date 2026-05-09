package importers

import (
	"strings"
	"testing"
)

func TestForDispatch(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantErr  bool
	}{
		{"", "csv", false},          // default
		{"csv", "csv", false},
		{"CSV", "csv", false},       // case-insensitive
		{" csv ", "csv", false},     // trim-tolerant
		{"linkedin", "linkedin", false},
		{"linkedin_csv", "linkedin", false},
		{"naukri", "naukri", false},
		{"naukri_csv", "naukri", false},
		{"unknown", "", true},
		{"gmail", "", true}, // gmail isn't built yet
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			imp, err := For(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("For(%q): expected error, got nil", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("For(%q): unexpected error: %v", c.in, err)
			}
			if imp.Name() != c.wantName {
				t.Errorf("For(%q): got %s, want %s", c.in, imp.Name(), c.wantName)
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"company", "company"},
		{"Company", "company"},
		{"Company Name", "companyname"},
		{"company_name", "companyname"},
		{"company-name", "companyname"},
		{" Company Name ", "companyname"},
		{"  COMPANY  NAME  ", "companyname"},
	}
	for _, c := range cases {
		if got := normalizeKey(c.in); got != c.want {
			t.Errorf("normalizeKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeDate(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},                       // empty is fine
		{"2026-05-09", "2026-05-09", true},  // ISO straight through
		{"05/09/2026", "2026-05-09", true},  // US format (LinkedIn)
		{"2026/05/09", "2026-05-09", true},
		{"May 9, 2026", "2026-05-09", true},
		{"January 2, 2026", "2026-01-02", true},
		{"02-Jan-2026", "2026-01-02", true},
		{"2026-05-09T10:30:00Z", "2026-05-09", true},
		{"not a date", "", false},
		{"99/99/99", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := normalizeDate(c.in)
			if ok != c.ok {
				t.Errorf("normalizeDate(%q): got ok=%v, want %v", c.in, ok, c.ok)
			}
			if got != c.want {
				t.Errorf("normalizeDate(%q): got %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCSVImporterMissingColumn(t *testing.T) {
	imp := &csvImporter{}
	// Missing the required `role` column.
	_, _, err := imp.Parse(strings.NewReader("company\nGoogle\nStripe\n"))
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Errorf("expected missing-column error mentioning role, got %v", err)
	}
}

func TestCSVImporterEmpty(t *testing.T) {
	imp := &csvImporter{}
	_, _, err := imp.Parse(strings.NewReader(""))
	if err == nil {
		t.Error("expected error on empty CSV")
	}
}

func TestCSVImporterBasic(t *testing.T) {
	csv := "company,role,stage,appliedAt\n" +
		"Stripe,Backend Engineer,APPLIED,2026-05-09\n" +
		"Linear,iOS Engineer,INTERESTED,\n"
	rows, errs, err := (&csvImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected per-row errors: %v", errs)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Company != "Stripe" || rows[0].Role != "Backend Engineer" || rows[0].Stage != "APPLIED" {
		t.Errorf("row 0 mismapped: %+v", rows[0])
	}
	if rows[1].Stage != "INTERESTED" {
		t.Errorf("row 1 stage default mishandled: %+v", rows[1])
	}
}

func TestCSVImporterMissingRequired(t *testing.T) {
	// Header has both required columns but a row leaves company blank.
	// Should surface as a per-row error, not abort the whole import.
	csv := "company,role\n" +
		"Stripe,Backend\n" +
		",Frontend\n" +
		"Linear,iOS\n"
	rows, errs, err := (&csvImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 valid rows, got %d", len(rows))
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "company and role are required") {
		t.Errorf("expected one per-row error, got %v", errs)
	}
}

func TestLinkedInImporterRequiredColumns(t *testing.T) {
	// LinkedIn CSV with the canonical column names.
	csv := "Company Name,Job Title,Application Date,Status,Job Url,Location\n" +
		"Stripe,Senior Backend Engineer,05/09/2026,Submitted,https://linkedin.com/jobs/123,Remote\n" +
		"Linear,iOS Engineer,05/01/2026,Interviewing,,Bengaluru\n"
	rows, errs, err := (&linkedInImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected per-row errors: %v", errs)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Company != "Stripe" || rows[0].Role != "Senior Backend Engineer" {
		t.Errorf("row 0 mismapped: %+v", rows[0])
	}
	if rows[0].AppliedAt != "2026-05-09" {
		t.Errorf("row 0 date not normalised: got %q", rows[0].AppliedAt)
	}
	if rows[0].Source != "LinkedIn" {
		t.Errorf("row 0 should be source=LinkedIn, got %q", rows[0].Source)
	}
	if rows[0].Stage != "APPLIED" {
		t.Errorf("row 0 status 'Submitted' should map to APPLIED, got %q", rows[0].Stage)
	}
	if rows[1].Stage != "TECHNICAL" {
		t.Errorf("row 1 status 'Interviewing' should map to TECHNICAL, got %q", rows[1].Stage)
	}
}

func TestLinkedInImporterColumnVariants(t *testing.T) {
	// Same content, different (older) column names.
	csv := "Company,Position,Date Applied,Application Status\n" +
		"Stripe,SDE-2,2026-05-09,Submitted\n"
	rows, _, err := (&linkedInImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(rows) != 1 || rows[0].Company != "Stripe" || rows[0].Role != "SDE-2" {
		t.Errorf("variant column mapping failed: %+v", rows)
	}
}

func TestLinkedInImporterMissingRequired(t *testing.T) {
	// Header exists but has neither Company-ish nor Title-ish column.
	csv := "ApplicationId,Date\n123,2026-05-09\n"
	_, _, err := (&linkedInImporter{}).Parse(strings.NewReader(csv))
	if err == nil {
		t.Error("expected fatal error for missing Company/Job Title")
	}
}

func TestNaukriImporterBasic(t *testing.T) {
	csv := "Company,Designation,Job Location,Application Date,Application Status,Salary\n" +
		"Razorpay,Senior Engineer,Bangalore,2026-05-08,Shortlisted,30 LPA\n"
	rows, errs, err := (&naukriImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected per-row errors: %v", errs)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Company != "Razorpay" || r.Role != "Senior Engineer" || r.Source != "Naukri" {
		t.Errorf("naukri row mismapped: %+v", r)
	}
	if r.Stage != "PHONE_SCREEN" {
		t.Errorf("Shortlisted should map to PHONE_SCREEN, got %q", r.Stage)
	}
	if r.SalaryRange != "30 LPA" {
		t.Errorf("salary mismapped: %q", r.SalaryRange)
	}
	if r.AppliedAt != "2026-05-08" {
		t.Errorf("date not normalised: %q", r.AppliedAt)
	}
}

func TestImporterBadDateSurfacesAsRowError(t *testing.T) {
	// Bad date should be reported as a per-row warning, not abort the row.
	csv := "Company Name,Job Title,Application Date\n" +
		"Stripe,SDE-2,not a date\n"
	rows, errs, err := (&linkedInImporter{}).Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("row should still land despite bad date")
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "unrecognised date") {
		t.Errorf("expected one date-warning error, got %v", errs)
	}
	if rows[0].AppliedAt != "" {
		t.Errorf("AppliedAt should be empty when date didn't parse, got %q", rows[0].AppliedAt)
	}
}
