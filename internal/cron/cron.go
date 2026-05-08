// Package cron is the in-process scheduled-job runner for sypher-api.
//
// One goroutine per Job; each loop computes its own NextFire and parks on
// time.After until either fire-time or ctx-cancel. No third-party deps,
// no cron-syntax parser, no shared mutex — just per-job scheduling.
//
// Multi-pod note: this package assumes single-pod execution. If we ever
// run two replicas, the second will run the same jobs at the same time.
// The fix at that point is a `cron_locks` table + acquire-then-run wrapper
// — additive, no refactor of this package. See ADR-001 D3.
package cron

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job describes a single scheduled task.
//
// NextFire is intentionally a function rather than a duration so each job
// can implement its own clock policy: "every day at 09:00 IST", "every
// 30s", "first day of month at midnight UTC". The Runner doesn't care.
type Job struct {
	// Name is used in log lines and metrics. Keep it short and stable —
	// "stale-apps", "daily-digest", "weekly-export". No spaces.
	Name string

	// NextFire returns the absolute time at which Run should be called
	// next, given the current wall clock. A job that wants to fire every
	// day at 09:00 returns "today 09:00 if it's still ahead, else tomorrow
	// 09:00". Pure function — no side effects.
	NextFire func(now time.Time) time.Time

	// Run is the work. It receives a context that's cancelled on shutdown;
	// long-running work should respect it. Errors are logged but don't
	// stop the loop — the next NextFire still fires.
	Run func(ctx context.Context) error
}

// Runner owns the lifecycle of a set of jobs. Construct with New, attach
// to a parent context with Start, and block on Wait during graceful
// shutdown to let in-flight Run calls finish.
type Runner struct {
	jobs   []Job
	logger *slog.Logger
	wg     sync.WaitGroup
}

func New(logger *slog.Logger, jobs ...Job) *Runner {
	return &Runner{jobs: jobs, logger: logger}
}

// Start spawns one goroutine per job. Returns immediately. Cancel ctx
// (e.g. on SIGTERM) to stop all loops; in-flight Run calls finish before
// their goroutine exits.
func (r *Runner) Start(ctx context.Context) {
	for _, j := range r.jobs {
		j := j // pin loop var
		r.wg.Add(1)
		go r.loop(ctx, j)
	}
}

// Wait blocks until every job goroutine has exited. Call this from the
// graceful-shutdown path so the api process doesn't die mid-job.
func (r *Runner) Wait() {
	r.wg.Wait()
}

// loop is one job's lifetime. Runs until ctx is done.
func (r *Runner) loop(ctx context.Context, j Job) {
	defer r.wg.Done()

	r.logger.Info("cron: starting", "job", j.Name)
	for {
		next := j.NextFire(time.Now())
		wait := time.Until(next)
		if wait < 0 {
			// Clock went backwards or NextFire returned the past — try
			// again in 1s to avoid a hot loop.
			wait = time.Second
		}
		r.logger.Info("cron: scheduled", "job", j.Name, "next_fire", next.Format(time.RFC3339), "wait", wait.Round(time.Second).String())

		select {
		case <-ctx.Done():
			r.logger.Info("cron: stopping", "job", j.Name, "reason", "context cancelled")
			return
		case <-time.After(wait):
			start := time.Now()
			r.logger.Info("cron: running", "job", j.Name, "started_at", start.Format(time.RFC3339))
			if err := j.Run(ctx); err != nil {
				r.logger.Error("cron: failed", "job", j.Name, "err", err, "duration", time.Since(start).Round(time.Millisecond).String())
			} else {
				r.logger.Info("cron: completed", "job", j.Name, "duration", time.Since(start).Round(time.Millisecond).String())
			}
			// Loop back. NextFire is idempotent so even if Run took longer
			// than the schedule interval (e.g. 9:00 job ran 5 minutes), the
			// next fire is "tomorrow 9:00", not "right now".
		}
	}
}
