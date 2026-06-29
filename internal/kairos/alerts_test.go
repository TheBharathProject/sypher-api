package kairos

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/kairos/marketdata"
	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
)

// TestAlertTrigger pins the rule semantics: strict comparisons, price_*
// rules read last, pct_change_* rules read changePct.
func TestAlertTrigger(t *testing.T) {
	cases := []struct {
		rule                       string
		threshold, last, changePct float64
		want                       bool
	}{
		{"price_above", 100, 101, 0, true},
		{"price_above", 100, 99, 0, false},
		{"price_below", 100, 99, 0, true},
		{"pct_change_above", 2, 0, 2.5, true},
		{"pct_change_below", -2, 0, -3.0, true},
		{"pct_change_below", -2, 0, -1.0, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%v", tc.rule, tc.threshold), func(t *testing.T) {
			got := AlertTriggered(tc.rule, tc.threshold, tc.last, tc.changePct)
			if got != tc.want {
				t.Errorf("AlertTriggered(%q, %v, %v, %v) = %v, want %v",
					tc.rule, tc.threshold, tc.last, tc.changePct, got, tc.want)
			}
		})
	}
}

// TestAlertTriggeredUnknownRule: an unknown rule never fires (defensive —
// the handler whitelists rules, but the evaluator shouldn't trust rows).
func TestAlertTriggeredUnknownRule(t *testing.T) {
	if AlertTriggered("bogus_rule", 0, 1, 1) {
		t.Error("unknown rule must not trigger")
	}
}

// ── EvaluateAlerts fakes ─────────────────────────────────────────────────

type fakeAlertStore struct {
	alerts     []ActiveAlert
	triggered  []uuid.UUID
	stamped    []uuid.UUID
	runs       []fakeRun
	loadErr    error
	triggerErr error
}

type fakeRun struct {
	job    string
	ok     bool
	rows   int
	detail string
}

func (f *fakeAlertStore) ActiveAlerts(context.Context) ([]ActiveAlert, error) {
	return f.alerts, f.loadErr
}

func (f *fakeAlertStore) TriggerAlert(_ context.Context, id uuid.UUID) error {
	if f.triggerErr != nil {
		return f.triggerErr
	}
	f.triggered = append(f.triggered, id)
	return nil
}

func (f *fakeAlertStore) StampEvaluated(_ context.Context, ids []uuid.UUID) error {
	f.stamped = append(f.stamped, ids...)
	return nil
}

func (f *fakeAlertStore) RecordIngestRun(_ context.Context, job string, _, _ time.Time, ok bool, rows int, detail string) error {
	f.runs = append(f.runs, fakeRun{job: job, ok: ok, rows: rows, detail: detail})
	return nil
}

type fakeQuotes struct {
	quotes  []provider.Quote
	err     error
	symbols []string // captured request
	calls   int
}

func (f *fakeQuotes) Quotes(_ context.Context, symbols []string) ([]provider.Quote, error) {
	f.calls++
	f.symbols = append([]string(nil), symbols...)
	return f.quotes, f.err
}

type fakeMailer struct {
	sent []mailer.Message
}

func (f *fakeMailer) Send(_ context.Context, in mailer.Message) error {
	f.sent = append(f.sent, in)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestEvaluateAlerts walks the full sweep: two users with alerts on two
// symbols → ONE batched quotes call, the breached alert flips + emails
// (user has notifications on), the quiet one is only stamped, and the
// sweep is recorded under job "alerts-sweep".
func TestEvaluateAlerts(t *testing.T) {
	hit := uuid.New()
	quiet := uuid.New()
	muted := uuid.New()
	store := &fakeAlertStore{alerts: []ActiveAlert{
		{ID: hit, Symbol: "RELIANCE", Rule: "price_above", Threshold: 2500, Email: "a@x.in", EmailEnabled: true},
		{ID: quiet, Symbol: "TCS", Rule: "price_below", Threshold: 3000, Email: "a@x.in", EmailEnabled: true},
		{ID: muted, Symbol: "RELIANCE", Rule: "pct_change_above", Threshold: 1, Email: "b@x.in", EmailEnabled: false},
	}}
	md := &fakeQuotes{quotes: []provider.Quote{
		{Symbol: "RELIANCE", Last: 2503.25, ChangePct: 1.4},
		{Symbol: "TCS", Last: 3100, ChangePct: 0.2},
	}}
	mail := &fakeMailer{}

	if err := EvaluateAlerts(context.Background(), store, md, mail, testLogger()); err != nil {
		t.Fatalf("EvaluateAlerts: %v", err)
	}

	if md.calls != 1 {
		t.Errorf("quotes calls = %d, want 1 (batched)", md.calls)
	}
	if len(md.symbols) != 2 {
		t.Errorf("quotes symbols = %v, want 2 unique symbols", md.symbols)
	}
	// hit and muted both breach; muted gets no email.
	if len(store.triggered) != 2 {
		t.Fatalf("triggered = %v, want [hit muted]", store.triggered)
	}
	if len(mail.sent) != 1 {
		t.Fatalf("emails sent = %d, want 1", len(mail.sent))
	}
	if got, want := mail.sent[0].Subject, "Kairos alert: RELIANCE price_above 2500"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	if mail.sent[0].To != "a@x.in" {
		t.Errorf("to = %q, want a@x.in", mail.sent[0].To)
	}
	// Everything loaded gets last_evaluated_at, breached or not.
	if len(store.stamped) != 3 {
		t.Errorf("stamped = %d ids, want 3", len(store.stamped))
	}
	if len(store.runs) != 1 || store.runs[0].job != "alerts-sweep" || !store.runs[0].ok {
		t.Errorf("ingest runs = %+v, want one ok alerts-sweep row", store.runs)
	}
	if store.runs[0].rows != 2 {
		t.Errorf("run rows = %d, want 2 (triggered count)", store.runs[0].rows)
	}
}

// TestEvaluateAlertsUnsupported: the null provider has no quotes
// capability — the sweep logs, records a skipped run, and returns nil
// (no error spam from the cron every 60s).
func TestEvaluateAlertsUnsupported(t *testing.T) {
	store := &fakeAlertStore{alerts: []ActiveAlert{
		{ID: uuid.New(), Symbol: "RELIANCE", Rule: "price_above", Threshold: 1},
	}}
	md := &fakeQuotes{err: fmt.Errorf("%w: quotes (provider null)", marketdata.ErrUnsupported)}
	mail := &fakeMailer{}

	if err := EvaluateAlerts(context.Background(), store, md, mail, testLogger()); err != nil {
		t.Fatalf("EvaluateAlerts must swallow ErrUnsupported, got %v", err)
	}
	if len(store.triggered) != 0 || len(store.stamped) != 0 {
		t.Error("nothing should trigger or stamp without quotes")
	}
	if len(store.runs) != 1 || !store.runs[0].ok || store.runs[0].detail == "" {
		t.Errorf("runs = %+v, want one ok skipped row with detail", store.runs)
	}
}

// TestEvaluateAlertsNoActive: an empty sweep still records its run.
func TestEvaluateAlertsNoActive(t *testing.T) {
	store := &fakeAlertStore{}
	md := &fakeQuotes{}
	if err := EvaluateAlerts(context.Background(), store, md, &fakeMailer{}, testLogger()); err != nil {
		t.Fatalf("EvaluateAlerts: %v", err)
	}
	if md.calls != 0 {
		t.Error("no active alerts must not call quotes")
	}
	if len(store.runs) != 1 || !store.runs[0].ok {
		t.Errorf("runs = %+v, want one ok row", store.runs)
	}
}
