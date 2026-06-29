package kairos

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// fakeRowSource feeds a fixed slice of ExportRows, optionally failing
// after the slice is exhausted — a stand-in for the pgx-backed
// ChainRowSource so the CSV writer is testable without a database.
type fakeRowSource struct {
	rows []ExportRow
	err  error
	i    int
}

func (f *fakeRowSource) Next() (ExportRow, bool, error) {
	if f.i >= len(f.rows) {
		return ExportRow{}, false, f.err
	}
	r := f.rows[f.i]
	f.i++
	return r, true, nil
}

func TestStreamChainCSVGolden(t *testing.T) {
	snap := time.Date(2026, 6, 12, 9, 15, 0, 0, time.UTC)
	expiry := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	src := &fakeRowSource{rows: []ExportRow{
		{
			Underlying:   "NIFTY",
			ExpiryDate:   expiry,
			Strike:       24000,
			OptionType:   "CE",
			SnapshotTime: snap,
			Spot:         24010.5,
			LTP:          120.25,
			Bid:          120,
			Ask:          120.5,
			OI:           5000000,
			OIChange:     250000,
			Volume:       1200000,
		},
		{
			Underlying:   "NIFTY",
			ExpiryDate:   expiry,
			Strike:       24000,
			OptionType:   "PE",
			SnapshotTime: snap,
			Spot:         24010.5,
			LTP:          98.7,
			Bid:          98.55,
			Ask:          98.9,
			OI:           4500000,
			OIChange:     -50000,
			Volume:       900000,
		},
	}}

	var buf bytes.Buffer
	n, err := StreamChainCSV(&buf, src)
	if err != nil {
		t.Fatalf("StreamChainCSV: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows written = %d, want 2", n)
	}

	want := "underlying,expiry_date,strike,option_type,snapshot_time,spot,ltp,bid,ask,oi,oi_change,volume\n" +
		"NIFTY,2026-06-18,24000,CE,2026-06-12T09:15:00Z,24010.5,120.25,120,120.5,5000000,250000,1200000\n" +
		"NIFTY,2026-06-18,24000,PE,2026-06-12T09:15:00Z,24010.5,98.7,98.55,98.9,4500000,-50000,900000\n"
	if got := buf.String(); got != want {
		t.Errorf("CSV mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestStreamChainCSVEmpty(t *testing.T) {
	var buf bytes.Buffer
	n, err := StreamChainCSV(&buf, &fakeRowSource{})
	if err != nil {
		t.Fatalf("StreamChainCSV: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows written = %d, want 0", n)
	}
	want := "underlying,expiry_date,strike,option_type,snapshot_time,spot,ltp,bid,ask,oi,oi_change,volume\n"
	if got := buf.String(); got != want {
		t.Errorf("CSV mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestStreamChainCSVSourceError(t *testing.T) {
	boom := errors.New("boom")
	src := &fakeRowSource{
		rows: []ExportRow{{Underlying: "NIFTY", OptionType: "CE"}},
		err:  boom,
	}
	var buf bytes.Buffer
	n, err := StreamChainCSV(&buf, src)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n != 1 {
		t.Fatalf("rows written = %d, want 1", n)
	}
}
