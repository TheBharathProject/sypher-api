package kairos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// ─────────────────────────────────────────────────────────────────────────
// Alerts (spec §3.4) — types + the 60s evaluator sweep.
//
// CRUD lives in handlers_alerts.go / store_alerts.go; the cron entry
// point is jobs/kairos_alerts.go, which gates on IST market hours and
// calls EvaluateAlerts.
// ─────────────────────────────────────────────────────────────────────────

// Alert mirrors one kairos.alerts row (migration 0027). JSON tags are
// camelCase and must match ApiAlert in kairos/lib/kairos-api.ts exactly
// — the FE client file is the contract. TriggeredAt / LastEvaluatedAt
// are optional there (triggeredAt?: string), hence pointers + omitempty.
type Alert struct {
	ID              uuid.UUID  `json:"id"`
	Symbol          string     `json:"symbol"`
	Rule            string     `json:"rule"`
	Threshold       float64    `json:"threshold"`
	Status          string     `json:"status"`
	TriggeredAt     *time.Time `json:"triggeredAt,omitempty"`
	LastEvaluatedAt *time.Time `json:"lastEvaluatedAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

// ActiveAlert is the evaluator's working row: the rule fields plus the
// owner's email and email_notifications_enabled flag, joined in one
// query so the sweep never does per-alert user lookups. Internal only —
// never serialized to the FE.
type ActiveAlert struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Symbol       string
	Rule         string
	Threshold    float64
	Email        string
	EmailEnabled bool
}

// Rule / status vocabularies — mirror the CHECK constraints in migration
// 0027 and ApiAlertRule / ApiAlertStatus in kairos-api.ts.
var validAlertRules = map[string]bool{
	"price_above":      true,
	"price_below":      true,
	"pct_change_above": true,
	"pct_change_below": true,
}

var validAlertStatuses = map[string]bool{
	"active":    true,
	"triggered": true,
	"paused":    true,
}

// alertsSweepJob is the kairos.ingest_runs job name for evaluator runs.
const alertsSweepJob = "alerts-sweep"

// AlertTriggered reports whether a rule has breached. Comparisons are
// strict — "above 100" means > 100, not ≥ — and an unknown rule never
// fires (the handler whitelists rules, but the evaluator doesn't trust
// rows). price_* rules read the last traded price; pct_change_* rules
// read the day's percent change.
func AlertTriggered(rule string, threshold, last, changePct float64) bool {
	switch rule {
	case "price_above":
		return last > threshold
	case "price_below":
		return last < threshold
	case "pct_change_above":
		return changePct > threshold
	case "pct_change_below":
		return changePct < threshold
	}
	return false
}

// alertSweepStore is the slice of *Store the evaluator needs — narrow so
// tests fake it (jobs-package idiom, same as reminderStore).
type alertSweepStore interface {
	ActiveAlerts(ctx context.Context) ([]ActiveAlert, error)
	TriggerAlert(ctx context.Context, id uuid.UUID) error
	StampEvaluated(ctx context.Context, ids []uuid.UUID) error
	RecordIngestRun(ctx context.Context, job string, startedAt, finishedAt time.Time, ok bool, rowsWritten int, detail string) error
}

// alertQuoteSource is the slice of *marketdata.Service the evaluator
// needs: the one batched quotes call.
type alertQuoteSource interface {
	Quotes(ctx context.Context, symbols []string) ([]provider.Quote, error)
}

// EvaluateAlerts runs one sweep over every user's active alerts:
//
//  1. load active alerts (joined with owner email + notification flag),
//  2. ONE batched Quotes call over the distinct symbols,
//  3. breached rule → status='triggered' + triggered_at, plus an email
//     when the owner has email notifications enabled,
//  4. every loaded alert gets last_evaluated_at stamped, breached or not,
//  5. the run lands in kairos.ingest_runs as job "alerts-sweep"
//     (rows_written = alerts triggered).
//
// A provider without the quotes capability (null/dhan/upstox/angel) is a
// skip, not an error: log once at info, record the run with a "skipped"
// detail, return nil — the cron fires every 60s and must not spam.
//
// Per-alert trigger/email failures are logged and skipped so one bad row
// can't stall the rest of the sweep.
func EvaluateAlerts(ctx context.Context, store alertSweepStore, md alertQuoteSource, mail mailer.Mailer, logger *slog.Logger) error {
	startedAt := time.Now().UTC()

	alerts, err := store.ActiveAlerts(ctx)
	if err != nil {
		recordAlertSweep(ctx, store, logger, startedAt, false, 0, "load active alerts: "+err.Error())
		return fmt.Errorf("alerts sweep: load active alerts: %w", err)
	}
	if len(alerts) == 0 {
		recordAlertSweep(ctx, store, logger, startedAt, true, 0, "")
		return nil
	}

	// Distinct symbols, sorted — one batched call, and a stable list so
	// marketdata's quote cache key recurs across ticks.
	seen := make(map[string]bool, len(alerts))
	symbols := make([]string, 0, len(alerts))
	for _, a := range alerts {
		if a.Symbol == "" || seen[a.Symbol] {
			continue
		}
		seen[a.Symbol] = true
		symbols = append(symbols, a.Symbol)
	}
	sort.Strings(symbols)

	quotes, err := md.Quotes(ctx, symbols)
	if err != nil {
		if errors.Is(err, marketdata.ErrUnsupported) {
			logger.Info("kairos alerts sweep skipped", "reason", err.Error())
			recordAlertSweep(ctx, store, logger, startedAt, true, 0, "skipped: "+err.Error())
			return nil
		}
		recordAlertSweep(ctx, store, logger, startedAt, false, 0, "quotes: "+err.Error())
		return fmt.Errorf("alerts sweep: quotes: %w", err)
	}
	bySymbol := make(map[string]provider.Quote, len(quotes))
	for _, q := range quotes {
		bySymbol[q.Symbol] = q
	}

	evaluated := make([]uuid.UUID, 0, len(alerts))
	triggered := 0
	for _, a := range alerts {
		evaluated = append(evaluated, a.ID)
		q, ok := bySymbol[a.Symbol]
		if !ok {
			// Provider didn't recognise the symbol (delisted, typo that
			// predates validation, index without a quote). Stamped but
			// never triggered.
			continue
		}
		if !AlertTriggered(a.Rule, a.Threshold, q.Last, q.ChangePct) {
			continue
		}
		if terr := store.TriggerAlert(ctx, a.ID); terr != nil {
			logger.Error("kairos alert trigger update failed", "alert_id", a.ID, "err", terr)
			continue
		}
		triggered++
		if a.EmailEnabled && a.Email != "" {
			sendAlertEmail(ctx, mail, logger, a, q)
		}
	}

	if serr := store.StampEvaluated(ctx, evaluated); serr != nil {
		logger.Error("kairos alerts stamp evaluated failed", "err", serr)
	}
	recordAlertSweep(ctx, store, logger, startedAt, true, triggered, "")
	return nil
}

// recordAlertSweep writes the ingest_runs audit row. Best-effort on a
// detached context (same idiom as SnapshotChains) — a failed audit write
// is logged, never escalated.
func recordAlertSweep(ctx context.Context, store alertSweepStore, logger *slog.Logger, startedAt time.Time, ok bool, triggered int, detail string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.RecordIngestRun(rctx, alertsSweepJob, startedAt, time.Now().UTC(), ok, triggered, detail); err != nil {
		logger.Warn("kairos ingest run record failed", "job", alertsSweepJob, "err", err)
	}
}

// sendAlertEmail ships the trigger notification. Subject format is part
// of the task contract: "Kairos alert: {SYM} {rule} {threshold}".
// Send failures are logged and swallowed — the alert is already flipped
// to triggered and the FE feed shows it regardless.
func sendAlertEmail(ctx context.Context, mail mailer.Mailer, logger *slog.Logger, a ActiveAlert, q provider.Quote) {
	th := formatThreshold(a.Threshold)
	msg := mailer.Message{
		To:      a.Email,
		Subject: fmt.Sprintf("Kairos alert: %s %s %s", a.Symbol, a.Rule, th),
		Text: fmt.Sprintf(
			"Your Kairos alert on %s (%s %s) just triggered.\nLast price: %.2f (%+.2f%% today).\n",
			a.Symbol, a.Rule, th, q.Last, q.ChangePct),
		HTML: fmt.Sprintf(
			"<p>Your Kairos alert on <strong>%s</strong> (%s %s) just triggered.</p><p>Last price: %.2f (%+.2f%% today).</p>",
			a.Symbol, a.Rule, th, q.Last, q.ChangePct),
	}
	if err := mail.Send(ctx, msg); err != nil {
		logger.Warn("kairos alert email failed", "alert_id", a.ID, "to", a.Email, "err", err)
	}
}

// formatThreshold renders a threshold the way a human typed it — "2500"
// not "2500.000000", "2.5" not "2.50" (thresholds are NUMERIC(12,2), so
// the round-trip is exact at this precision).
func formatThreshold(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
