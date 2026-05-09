package importers

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// csvImporter reads the canonical Pegasus CSV (the shape
// ApplicationsTemplate emits). Required columns: company, role.
// Everything else optional. Same column set + behaviour as the
// pre-Phase-4 import handler — moved here for the source-discriminator
// pattern from ADR-004.
type csvImporter struct{}

func (csvImporter) Name() string { return "csv" }

func (csvImporter) Parse(r io.Reader) ([]ApplicationInput, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we look up by name

	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("empty CSV")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}

	idx := indexHeader(header)
	for _, k := range []string{"company", "role"} {
		if _, ok := idx[k]; !ok {
			return nil, nil, fmt.Errorf("missing required column: %s", k)
		}
	}

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
		get := getter(idx, rec)
		in := ApplicationInput{
			Company:        get("company"),
			Role:           get("role"),
			Source:         get("source"),
			Location:       get("location"),
			SalaryRange:    get("salaryRange"),
			Stage:          orDefault(get("stage"), "INTERESTED"),
			AppliedAt:      get("appliedAt"),
			ApplyDeadline:  get("applyDeadline"),
			JobLink:        get("jobLink"),
			JobDescription: get("description"),
			Notes:          get("notes"),
			Stale:          strings.EqualFold(get("stale"), "true"),
		}
		if in.Company == "" || in.Role == "" {
			errs = append(errs, fmt.Sprintf("line %d: company and role are required", line))
			continue
		}
		rows = append(rows, in)
	}
	return rows, errs, nil
}
