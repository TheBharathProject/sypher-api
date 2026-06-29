package kairos

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// Chain CSV export (spec §3.7) — the "download the historical options
// data" requirement. StreamChainCSV is pure (io.Writer + RowSource) so
// the golden test runs without a database; the pgx-backed source lives
// in store_admin.go and the HTTP/gzip plumbing in handlers_admin.go.

// exportHeader matches kairos.option_chains' queryable columns
// (migration 0025), minus the synthetic id and the provider tag.
var exportHeader = []string{
	"underlying", "expiry_date", "strike", "option_type", "snapshot_time",
	"spot", "ltp", "bid", "ask", "oi", "oi_change", "volume",
}

// exportFlushEvery is how many data rows go between flushes. Each flush
// drains the csv buffer and — when the destination supports it — pushes
// gzip + HTTP buffers too, so a multi-million-row export streams to the
// browser instead of accumulating server-side.
const exportFlushEvery = 5000

// ExportRow is one CSV line of a chain export — the COALESCE'd column
// values of one kairos.option_chains row.
type ExportRow struct {
	Underlying   string
	ExpiryDate   time.Time
	Strike       int
	OptionType   string
	SnapshotTime time.Time
	Spot         float64
	LTP          float64
	Bid          float64
	Ask          float64
	OI           int64
	OIChange     int64
	Volume       int64
}

// fields renders the row in exportHeader order. Floats use the shortest
// round-trip representation ("120" not "120.00"); times are UTC RFC3339.
func (r ExportRow) fields() []string {
	return []string{
		r.Underlying,
		r.ExpiryDate.Format("2006-01-02"),
		strconv.Itoa(r.Strike),
		r.OptionType,
		r.SnapshotTime.UTC().Format(time.RFC3339),
		strconv.FormatFloat(r.Spot, 'f', -1, 64),
		strconv.FormatFloat(r.LTP, 'f', -1, 64),
		strconv.FormatFloat(r.Bid, 'f', -1, 64),
		strconv.FormatFloat(r.Ask, 'f', -1, 64),
		strconv.FormatInt(r.OI, 10),
		strconv.FormatInt(r.OIChange, 10),
		strconv.FormatInt(r.Volume, 10),
	}
}

// RowSource yields export rows one at a time. The production
// implementation (ChainRowSource) wraps pgx.Rows; tests fake it with a
// slice. ok=false with err==nil means the source is cleanly exhausted.
type RowSource interface {
	Next() (row ExportRow, ok bool, err error)
}

// errFlusher is satisfied by gzip.Writer and by the handler's
// gzip+http.Flusher composite — anything whose buffers should be pushed
// downstream mid-stream.
type errFlusher interface {
	Flush() error
}

// StreamChainCSV writes the header then every row from src as CSV onto
// w, flushing every exportFlushEvery rows when w supports Flush().
// Returns the number of data rows written (header excluded) along with
// the first error from the source or the writer.
func StreamChainCSV(w io.Writer, src RowSource) (int, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write(exportHeader); err != nil {
		return 0, err
	}
	n := 0
	for {
		row, ok, err := src.Next()
		if err != nil {
			cw.Flush()
			return n, err
		}
		if !ok {
			break
		}
		if err := cw.Write(row.fields()); err != nil {
			return n, err
		}
		n++
		if n%exportFlushEvery == 0 {
			cw.Flush()
			if err := cw.Error(); err != nil {
				return n, err
			}
			if f, fok := w.(errFlusher); fok {
				if err := f.Flush(); err != nil {
					return n, err
				}
			}
		}
	}
	cw.Flush()
	return n, cw.Error()
}
