package kairos

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/admin/* (spec §3.7)
//
// All of these sit behind auth.RequireAdmin — wired in routes.go by the
// wiring task. Response shapes are dictated by the admin functions in
// kairos/lib/kairos-api.ts (camelCase, envelope keys); that file is the
// contract.
// ─────────────────────────────────────────────────────────────────────────

// adminSnapshotAge is one entry of AdminHealth's snapshots array.
type adminSnapshotAge struct {
	Underlying     string     `json:"underlying"`
	LastSnapshotAt *time.Time `json:"lastSnapshotAt,omitempty"`
	AgeSeconds     *int64     `json:"ageSeconds,omitempty"`
}

// adminHealthResponse mirrors ApiAdminHealth.
type adminHealthResponse struct {
	Provider            ProviderStatus     `json:"provider"`
	Snapshots           []adminSnapshotAge `json:"snapshots"`
	QueueDepth          int                `json:"queueDepth"`
	Crons               []CronRunStatus    `json:"crons"`
	DBSizeBytes         *int64             `json:"dbSizeBytes,omitempty"`
	PartitionsSizeBytes *int64             `json:"partitionsSizeBytes,omitempty"`
}

// AdminHealth is the system page: provider readiness, per-underlying
// snapshot age, worker queue depth, last cron run per job, and database
// + chain-partition sizes. Sub-queries are best-effort — a failing one
// is logged and its field omitted rather than failing the whole card.
func (h *Handler) AdminHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	ps := ProviderStatus{Active: h.provider.Name()}
	if err := h.provider.IsReady(ctx); err != nil {
		ps.Reason = err.Error()
	} else {
		ps.Ready = true
	}
	if lurl, ok := h.provider.(interface{ LoginURL() string }); ok {
		ps.LoginURL = lurl.LoginURL()
	}

	now := time.Now()
	last, err := h.store.LastSnapshotTimes(ctx)
	if err != nil {
		h.logger.Error("admin health: snapshot times failed", "err", err, "request_id", httpx.RequestID(ctx))
	}
	snapshots := make([]adminSnapshotAge, 0, 3)
	for _, u := range provider.AllUnderlyings() {
		entry := adminSnapshotAge{Underlying: string(u)}
		if t, ok := last[string(u)]; ok {
			t := t
			age := int64(now.Sub(t).Seconds())
			entry.LastSnapshotAt = &t
			entry.AgeSeconds = &age
		}
		snapshots = append(snapshots, entry)
	}

	depth := 0
	if h.worker != nil {
		depth = h.worker.Depth()
	}

	crons, err := h.store.LastIngestRunPerJob(ctx)
	if err != nil {
		h.logger.Error("admin health: ingest runs failed", "err", err, "request_id", httpx.RequestID(ctx))
	}
	if crons == nil {
		crons = []CronRunStatus{}
	}

	resp := adminHealthResponse{
		Provider:   ps,
		Snapshots:  snapshots,
		QueueDepth: depth,
		Crons:      crons,
	}
	if n, err := h.store.DatabaseSize(ctx); err != nil {
		h.logger.Error("admin health: db size failed", "err", err, "request_id", httpx.RequestID(ctx))
	} else {
		resp.DBSizeBytes = &n
	}
	if parts, err := h.store.PartitionInfo(ctx); err != nil {
		h.logger.Error("admin health: partition info failed", "err", err, "request_id", httpx.RequestID(ctx))
	} else {
		var total int64
		for _, p := range parts {
			total += p.SizeBytes
		}
		resp.PartitionsSizeBytes = &total
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// maxCoverageRangeDays caps the heatmap query — a year-and-change is
// plenty for the UI and bounds the zero-filled response size.
const maxCoverageRangeDays = 400

// AdminCoverage returns per-day snapshot counts for the heatmap.
// Days without data are zero-filled so the FE gets a contiguous range.
func (h *Handler) AdminCoverage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, ok := adminUnderlying(w, q.Get("underlying"))
	if !ok {
		return
	}
	from, to, ok := adminDateRange(w, q.Get("from"), q.Get("to"))
	if !ok {
		return
	}
	if daysBetween(from, to) > maxCoverageRangeDays {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input",
			fmt.Sprintf("coverage range is limited to %d days", maxCoverageRangeDays))
		return
	}
	fromStart, _ := utcDayBounds(from)
	_, toEnd := utcDayBounds(to)
	counts, err := h.store.Coverage(r.Context(), u, fromStart, toEnd)
	if err != nil {
		h.logger.Error("admin coverage failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	byDate := make(map[string]CoverageDayCount, len(counts))
	for _, c := range counts {
		byDate[c.Date] = c
	}
	days := make([]CoverageDayCount, 0, daysBetween(from, to))
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if c, ok := byDate[key]; ok {
			days = append(days, c)
		} else {
			days = append(days, CoverageDayCount{Date: key})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": days})
}

// AdminCoverageDay is the heatmap drilldown: per-expiry strike/snapshot
// counts plus the snapshot times recorded on one day.
func (h *Handler) AdminCoverageDay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, ok := adminUnderlying(w, q.Get("underlying"))
	if !ok {
		return
	}
	date, err := time.Parse("2006-01-02", q.Get("date"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "date must be YYYY-MM-DD")
		return
	}
	expiries, times, err := h.store.CoverageDay(r.Context(), u, date)
	if err != nil {
		h.logger.Error("admin coverage day failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if expiries == nil {
		expiries = []CoverageExpiry{}
	}
	snapshotTimes := make([]string, 0, len(times))
	for _, t := range times {
		snapshotTimes = append(snapshotTimes, t.UTC().Format(time.RFC3339))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"underlying":    string(u),
		"date":          date.Format("2006-01-02"),
		"expiries":      expiries,
		"snapshotTimes": snapshotTimes,
	})
}

// maxExportRangeDays caps a single chain export. 92 days ≈ one quarter;
// bigger pulls must be chunked so neither the DB cursor nor the
// download runs unbounded.
const maxExportRangeDays = 92

// gzipFlushWriter chains gzip → HTTP. StreamChainCSV calls Flush()
// every ~5k rows; we push the gzip buffer and then the HTTP response
// buffer so the browser sees steady download progress.
type gzipFlushWriter struct {
	gz *gzip.Writer
	rw http.ResponseWriter
}

func (g *gzipFlushWriter) Write(p []byte) (int, error) { return g.gz.Write(p) }

func (g *gzipFlushWriter) Flush() error {
	if err := g.gz.Flush(); err != nil {
		return err
	}
	if f, ok := g.rw.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// AdminExportChains streams the historical chain rows for an underlying
// + date range (optionally one expiry) as gzip CSV — the "download the
// data" endpoint. The whole pipeline is streaming: pgx rows → CSV →
// gzip → HTTP, flushed every ~5k rows. Errors after the first byte has
// been written can only be logged; the truncated download is the
// client's signal.
func (h *Handler) AdminExportChains(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, ok := adminUnderlying(w, q.Get("underlying"))
	if !ok {
		return
	}
	fromStr, toStr := q.Get("from"), q.Get("to")
	from, to, ok := adminDateRange(w, fromStr, toStr)
	if !ok {
		return
	}
	if daysBetween(from, to) > maxExportRangeDays {
		httpx.WriteError(w, http.StatusBadRequest, "range_too_large",
			fmt.Sprintf("export range is limited to %d days — chunk the export into smaller date ranges", maxExportRangeDays))
		return
	}
	var expiry *time.Time
	if e := q.Get("expiry"); e != "" {
		t, err := time.Parse("2006-01-02", e)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_input", "expiry must be YYYY-MM-DD")
			return
		}
		expiry = &t
	}

	fromStart, _ := utcDayBounds(from)
	_, toEnd := utcDayBounds(to)
	src, err := h.store.ChainExportRows(r.Context(), u, fromStart, toEnd, expiry)
	if err != nil {
		h.logger.Error("chain export query failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	defer src.Close()

	filename := fmt.Sprintf("chains_%s_%s_%s.csv.gz", string(u), fromStr, toStr)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	started := time.Now()
	gz := gzip.NewWriter(w)
	n, streamErr := StreamChainCSV(&gzipFlushWriter{gz: gz, rw: w}, src)
	if streamErr != nil {
		h.logger.Error("chain export stream failed", "err", streamErr, "rows", n,
			"request_id", httpx.RequestID(r.Context()))
	}
	if err := gz.Close(); err != nil && streamErr == nil {
		h.logger.Error("chain export gzip close failed", "err", err, "request_id", httpx.RequestID(r.Context()))
	}
	// Exports show up in the ingest-runs log too (spec §3.8). Recorded
	// on a non-cancellable context so a mid-download client disconnect
	// (which cancels r.Context()) still leaves an audit row.
	detail := fmt.Sprintf("%s %s..%s", string(u), fromStr, toStr)
	if streamErr != nil {
		detail += ": " + streamErr.Error()
	}
	if err := h.store.RecordIngestRun(context.WithoutCancel(r.Context()), "chain_export", started, time.Now(), streamErr == nil, n, detail); err != nil {
		h.logger.Warn("chain export ingest-run record failed", "err", err, "request_id", httpx.RequestID(r.Context()))
	}
}

// AdminIngestRuns lists recent ingest_runs rows, optionally filtered to
// one job.
func (h *Handler) AdminIngestRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if l := q.Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	runs, err := h.store.ListIngestRuns(r.Context(), q.Get("job"), limit)
	if err != nil {
		h.logger.Error("list ingest runs failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if runs == nil {
		runs = []IngestRun{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// AdminPartitions lists the option_chains child partitions with sizes
// and row estimates.
func (h *Handler) AdminPartitions(w http.ResponseWriter, r *http.Request) {
	parts, err := h.store.PartitionInfo(r.Context())
	if err != nil {
		h.logger.Error("partition info failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if parts == nil {
		parts = []Partition{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"partitions": parts})
}

var validBacktestStatuses = map[string]bool{
	"pending": true, "running": true, "done": true, "failed": true,
}

// AdminBacktests lists recent backtests across all users, optionally
// filtered by status — the admin queue/failed views.
func (h *Handler) AdminBacktests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !validBacktestStatuses[status] {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "status must be pending|running|done|failed")
		return
	}
	limit := 0
	if l := q.Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	out, err := h.store.AdminListBacktests(r.Context(), status, limit)
	if err != nil {
		h.logger.Error("admin list backtests failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if out == nil {
		out = []AdminBacktest{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"backtests": out})
}

// AdminRetryBacktest re-queues a failed backtest: failed → pending in
// the DB, then a non-blocking enqueue (channel full → next sweep).
func (h *Handler) AdminRetryBacktest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	if err := h.store.RetryBacktest(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "backtest not found or not in failed state")
			return
		}
		h.logger.Error("retry backtest failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if h.worker != nil {
		h.worker.Enqueue(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminUsers pages through all users (keyset, jobtracker pattern) with
// an optional email/name search.
func (h *Handler) AdminUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if l := q.Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	users, next, err := h.store.AdminListUsers(r.Context(), strings.TrimSpace(q.Get("q")), q.Get("cursor"), limit)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_cursor", "invalid cursor")
			return
		}
		h.logger.Error("admin list users failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	if users == nil {
		users = []AdminUser{}
	}
	resp := struct {
		Users      []AdminUser `json:"users"`
		NextCursor string      `json:"nextCursor,omitempty"`
	}{Users: users, NextCursor: next}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// AdminUpdateUser PATCHes the operator-controlled flags {isPremium?,
// isAdmin?} via auth.SetUserFlags and echoes the updated user.
func (h *Handler) AdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	if h.authStore == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "not_configured", "auth store is not configured")
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid id")
		return
	}
	var in struct {
		IsPremium *bool `json:"isPremium"`
		IsAdmin   *bool `json:"isAdmin"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.IsPremium == nil && in.IsAdmin == nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "nothing to update — send isPremium and/or isAdmin")
		return
	}
	if err := h.authStore.SetUserFlags(r.Context(), id, in.IsPremium, in.IsAdmin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		h.logger.Error("set user flags failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	out, err := h.store.AdminGetUser(r.Context(), id)
	if err != nil {
		h.logger.Error("load updated user failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// fundamentalsHeader is the exact CSV header the upload requires —
// spec §2's equity_fundamentals column names, in order.
var fundamentalsHeader = []string{
	"symbol", "name", "sector", "mkt_cap_cr", "pe", "roce", "roe", "de",
	"np_y1", "np_y2", "opm", "cagr3_profit", "cagr3_sales",
}

// maxFundamentalsUpload caps the multipart body. The screener universe
// is a few thousand symbols; 2 MiB of CSV is far more than enough.
const maxFundamentalsUpload = 2 << 20

type uploadRowError struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

// AdminUploadFundamentals ingests a fundamentals CSV (multipart field
// "file", ≤2 MiB) and upserts kairos.equity_fundamentals per row.
// Malformed rows are collected as {row, message} (1-based line numbers,
// header = line 1) instead of aborting the upload; the response is
// {rowsWritten, errors?} per ApiFundamentalsUploadResult.
func (h *Handler) AdminUploadFundamentals(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFundamentalsUpload)
	if err := r.ParseMultipartForm(maxFundamentalsUpload); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "could not parse multipart form (max 2 MiB)")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", `multipart field "file" is required`)
		return
	}
	defer file.Close()

	rd := csv.NewReader(file)
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "could not read CSV header")
		return
	}
	if !equalHeader(header, fundamentalsHeader) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input",
			"CSV header must be exactly: "+strings.Join(fundamentalsHeader, ","))
		return
	}

	upserted := 0
	var rowErrs []uploadRowError
	line := 1 // header consumed
	for {
		rec, err := rd.Read()
		line++
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, csv.ErrFieldCount) {
				rowErrs = append(rowErrs, uploadRowError{Row: line,
					Message: fmt.Sprintf("expected %d columns, got %d", len(fundamentalsHeader), len(rec))})
				continue
			}
			rowErrs = append(rowErrs, uploadRowError{Row: line, Message: "malformed CSV row"})
			break // parser state is unreliable after a non-count error
		}
		f, perr := parseFundamentalRow(rec)
		if perr != nil {
			rowErrs = append(rowErrs, uploadRowError{Row: line, Message: perr.Error()})
			continue
		}
		if err := h.store.AdminUpsertFundamental(r.Context(), *f); err != nil {
			h.logger.Error("fundamentals upsert failed", "err", err, "symbol", f.Symbol,
				"request_id", httpx.RequestID(r.Context()))
			rowErrs = append(rowErrs, uploadRowError{Row: line, Message: "database error — row not written"})
			continue
		}
		upserted++
	}

	resp := struct {
		RowsWritten int              `json:"rowsWritten"`
		Errors      []uploadRowError `json:"errors,omitempty"`
	}{RowsWritten: upserted, Errors: rowErrs}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// adminUnderlying validates the ?underlying= param. Writes 400 and
// returns ok=false on failure.
func adminUnderlying(w http.ResponseWriter, raw string) (provider.Underlying, bool) {
	u := strings.ToUpper(strings.TrimSpace(raw))
	if !validUnderlyings[u] {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "underlying must be NIFTY|BANKNIFTY|SENSEX")
		return "", false
	}
	return provider.Underlying(u), true
}

// adminDateRange parses ?from=&to= (YYYY-MM-DD, from ≤ to). Writes 400
// and returns ok=false on failure.
func adminDateRange(w http.ResponseWriter, fromStr, toStr string) (from, to time.Time, ok bool) {
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "from must be YYYY-MM-DD")
		return from, to, false
	}
	to, err = time.Parse("2006-01-02", toStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "to must be YYYY-MM-DD")
		return from, to, false
	}
	if from.After(to) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "from must be ≤ to")
		return from, to, false
	}
	return from, to, true
}

// daysBetween is the inclusive day count of [from, to].
func daysBetween(from, to time.Time) int {
	return int(to.Sub(from).Hours()/24) + 1
}

// equalHeader compares CSV headers field-by-field, tolerating
// surrounding whitespace and a UTF-8 BOM on the first field.
func equalHeader(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g := strings.TrimSpace(got[i])
		if i == 0 {
			g = strings.TrimPrefix(g, "\ufeff")
		}
		if g != want[i] {
			return false
		}
	}
	return true
}

// parseFundamentalRow converts one 13-field CSV record into a
// FundamentalUpsert. Empty numeric cells become NULL; anything
// unparseable is a row error.
func parseFundamentalRow(rec []string) (*FundamentalUpsert, error) {
	symbol := strings.ToUpper(strings.TrimSpace(rec[0]))
	if symbol == "" {
		return nil, errors.New("symbol is required")
	}
	if len(symbol) > 32 {
		return nil, errors.New("symbol exceeds 32 characters")
	}
	f := &FundamentalUpsert{
		Symbol: symbol,
		Name:   strings.TrimSpace(rec[1]),
		Sector: strings.TrimSpace(rec[2]),
	}
	numeric := []struct {
		idx  int
		name string
		dst  **float64
	}{
		{3, "mkt_cap_cr", &f.MktCapCr},
		{4, "pe", &f.PE},
		{5, "roce", &f.ROCE},
		{6, "roe", &f.ROE},
		{7, "de", &f.DE},
		{8, "np_y1", &f.NPY1},
		{9, "np_y2", &f.NPY2},
		{10, "opm", &f.OPM},
		{11, "cagr3_profit", &f.CAGR3Profit},
		{12, "cagr3_sales", &f.CAGR3Sales},
	}
	for _, n := range numeric {
		cell := strings.TrimSpace(rec[n.idx])
		if cell == "" {
			continue
		}
		v, err := strconv.ParseFloat(cell, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", n.name, cell)
		}
		*n.dst = &v
	}
	return f, nil
}
