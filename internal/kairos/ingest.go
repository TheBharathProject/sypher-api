package kairos

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// SnapshotResult is the per-underlying count of rows inserted in a
// snapshot pass. Used by both the cron job and the admin trigger so
// callers can log / surface what got ingested.
type SnapshotResult struct {
	Underlying string `json:"underlying"`
	Expiry     string `json:"expiry"`
	Rows       int64  `json:"rows"`
	Err        string `json:"err,omitempty"`
}

// SnapshotChains fetches the chain for each (underlying, near-expiry)
// pair and bulk-inserts into kairos.option_chains. Returns one
// SnapshotResult per (underlying, expiry) attempted.
//
// Caller is responsible for deciding whether to call this (market-hours
// check, provider.IsReady, etc.). This function just does the work,
// every time.
//
// Every invocation — cron tick or admin trigger — also records an
// "options-snapshot" row in kairos.ingest_runs (spec §3.8): total rows
// written, ok=false plus an error summary in detail when any
// (underlying, expiry) failed. Recording is best-effort: a failure to
// write the audit row is logged and never fails the snapshot itself.
func SnapshotChains(ctx context.Context, store *Store, prov provider.BrokerProvider, logger *slog.Logger) []SnapshotResult {
	const job = "options-snapshot"
	startedAt := time.Now().UTC()
	out := snapshotChains(ctx, store, prov, logger)
	finishedAt := time.Now().UTC()

	var rowsWritten int64
	var errs []string
	for _, r := range out {
		rowsWritten += r.Rows
		if r.Err != "" {
			label := r.Underlying
			if r.Expiry != "" {
				label += " " + r.Expiry
			}
			errs = append(errs, label+": "+r.Err)
		}
	}
	// Detached context so the audit row still lands when the caller's
	// ctx was cancelled mid-snapshot (the run did happen; log it).
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rerr := store.RecordIngestRun(rctx, job, startedAt, finishedAt,
		len(errs) == 0, int(rowsWritten), strings.Join(errs, "; ")); rerr != nil {
		logger.Warn("kairos ingest run record failed", "job", job, "err", rerr)
	}
	return out
}

// snapshotChains is the actual fetch+insert pass. Kept separate so the
// exported wrapper above can bracket it with ingest_runs bookkeeping.
func snapshotChains(ctx context.Context, store *Store, prov provider.BrokerProvider, logger *slog.Logger) []SnapshotResult {
	var out []SnapshotResult
	for _, u := range provider.AllUnderlyings() {
		exps, err := prov.FetchExpiries(ctx, u)
		if err != nil {
			out = append(out, SnapshotResult{
				Underlying: string(u),
				Err:        "expiries: " + err.Error(),
			})
			continue
		}
		// First 4 active expiries — bounds ingest cost and matches what
		// retail tools usually surface.
		active := filterActive(exps)
		if len(active) > 4 {
			active = active[:4]
		}
		for _, e := range active {
			rows, ferr := prov.FetchOptionChain(ctx, u, e.Date)
			if ferr != nil {
				logger.Warn("kairos chain fetch failed",
					"underlying", u,
					"expiry", e.Date.Format("2006-01-02"),
					"err", ferr.Error())
				out = append(out, SnapshotResult{
					Underlying: string(u),
					Expiry:     e.Date.Format("2006-01-02"),
					Err:        ferr.Error(),
				})
				continue
			}
			rows = TrimToATMBand(rows)
			n, ierr := store.BulkInsertChain(ctx, prov.Name(), rows)
			if ierr != nil {
				logger.Warn("kairos chain insert failed",
					"underlying", u,
					"expiry", e.Date.Format("2006-01-02"),
					"err", ierr.Error())
				out = append(out, SnapshotResult{
					Underlying: string(u),
					Expiry:     e.Date.Format("2006-01-02"),
					Err:        ierr.Error(),
				})
				continue
			}
			logger.Info("kairos chain inserted",
				"underlying", u,
				"expiry", e.Date.Format("2006-01-02"),
				"rows", n)
			out = append(out, SnapshotResult{
				Underlying: string(u),
				Expiry:     e.Date.Format("2006-01-02"),
				Rows:       n,
			})
		}
	}
	return out
}

// filterActive drops expiries already in the past.
func filterActive(exps []provider.Expiry) []provider.Expiry {
	var active []provider.Expiry
	for _, e := range exps {
		if e.Date.IsZero() {
			continue
		}
		if e.Date.Unix() < 0 {
			continue
		}
		active = append(active, e)
	}
	return active
}

// TrimToATMBand drops strikes outside ATM ± 30 to bound the per-snapshot
// row count. Spot is read from any row's Spot field (all rows in a
// snapshot share the spot).
//
// Returns the rows unchanged if spot is 0 or there are no rows.
// Exported so cron and admin paths both use the same trim.
func TrimToATMBand(rows []provider.ChainRow) []provider.ChainRow {
	if len(rows) == 0 || rows[0].Spot == 0 {
		return rows
	}
	spot := rows[0].Spot
	step := inferStep(rows)
	if step == 0 {
		return rows
	}
	atm := int(spot/float64(step)+0.5) * step
	low, high := atm-30*step, atm+30*step
	out := rows[:0]
	for _, r := range rows {
		if r.Strike >= low && r.Strike <= high {
			out = append(out, r)
		}
	}
	return out
}

func inferStep(rows []provider.ChainRow) int {
	uniq := map[int]struct{}{}
	for _, r := range rows {
		uniq[r.Strike] = struct{}{}
	}
	if len(uniq) < 2 {
		return 0
	}
	var sorted []int
	for k := range uniq {
		sorted = append(sorted, k)
	}
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	minDiff := 0
	for i := 1; i < len(sorted); i++ {
		d := sorted[i] - sorted[i-1]
		if d > 0 && (minDiff == 0 || d < minDiff) {
			minDiff = d
		}
	}
	return minDiff
}
