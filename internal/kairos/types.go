package kairos

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/kairos/provider"
)

// ─────────────────────────────────────────────────────────────────────────
// Strategy DSL — kept in sync with kairos/app/strategies/page.tsx
// ─────────────────────────────────────────────────────────────────────────

// Leg is one leg of a multi-leg options strategy. Stored as one element
// of the JSONB legs array (ADR-0012 D1).
//
// The schema validator (ValidateLegs) is the source of truth — adding a
// new field here means updating ValidateLegs.
type Leg struct {
	Side       string `json:"side"`        // BUY | SELL
	OptType    string `json:"optType"`     // CE | PE
	StrikeRule string `json:"strikeRule"`  // ATM | ATM±1 | ATM±2 | ATM±3
	ExpiryRule string `json:"expiryRule"`  // WEEKLY | NEXT_WEEKLY | MONTHLY
	Lots       int    `json:"lots"`
}

// Strategy is what a user saves and what the backtester replays.
type Strategy struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"-"`
	Name       string    `json:"name"`
	Underlying string    `json:"underlying"`
	Legs       []Leg     `json:"legs"`
	EntryTime  string    `json:"entryTime"`
	ExitTime   string    `json:"exitTime"`
	StopLoss   float64   `json:"stopLoss"`
	Target     float64   `json:"target"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

var (
	validSides       = map[string]bool{"BUY": true, "SELL": true}
	validOptTypes    = map[string]bool{"CE": true, "PE": true}
	validStrikeRules = map[string]bool{
		"ATM":   true,
		"ATM+1": true, "ATM+2": true, "ATM+3": true,
		"ATM-1": true, "ATM-2": true, "ATM-3": true,
	}
	validExpiryRules = map[string]bool{
		"WEEKLY": true, "NEXT_WEEKLY": true, "MONTHLY": true,
	}
	validUnderlyings = map[string]bool{
		"NIFTY": true, "BANKNIFTY": true, "SENSEX": true,
	}
)

// ValidateStrategy returns the first violation it finds. Called by the
// handler before any DB write. No DB-side CHECK constraint per
// ADR-0012 D4.
func ValidateStrategy(s *Strategy) error {
	if s.Name = strings.TrimSpace(s.Name); s.Name == "" {
		return errors.New("name is required")
	}
	if len(s.Name) > 80 {
		return errors.New("name exceeds 80 chars")
	}
	if !validUnderlyings[s.Underlying] {
		return fmt.Errorf("underlying %q must be one of NIFTY|BANKNIFTY|SENSEX", s.Underlying)
	}
	if err := ValidateLegs(s.Legs); err != nil {
		return err
	}
	if !isHHMM(s.EntryTime) {
		return errors.New("entryTime must be HH:MM")
	}
	if !isHHMM(s.ExitTime) {
		return errors.New("exitTime must be HH:MM")
	}
	if s.StopLoss < 0 || s.StopLoss > 200 {
		return errors.New("stopLoss must be 0-200")
	}
	if s.Target < 0 || s.Target > 500 {
		return errors.New("target must be 0-500")
	}
	return nil
}

// ValidateLegs returns a typed error if any leg is malformed. Each rule
// here matches the union types in kairos/app/strategies/page.tsx.
func ValidateLegs(legs []Leg) error {
	if len(legs) == 0 {
		return errors.New("at least one leg is required")
	}
	if len(legs) > 8 {
		return errors.New("at most 8 legs are supported")
	}
	for i, l := range legs {
		if !validSides[l.Side] {
			return fmt.Errorf("leg %d: side must be BUY or SELL", i)
		}
		if !validOptTypes[l.OptType] {
			return fmt.Errorf("leg %d: optType must be CE or PE", i)
		}
		if !validStrikeRules[l.StrikeRule] {
			return fmt.Errorf("leg %d: strikeRule must be ATM or ATM±N", i)
		}
		if !validExpiryRules[l.ExpiryRule] {
			return fmt.Errorf("leg %d: expiryRule must be WEEKLY|NEXT_WEEKLY|MONTHLY", i)
		}
		if l.Lots <= 0 || l.Lots > 100 {
			return fmt.Errorf("leg %d: lots must be 1-100", i)
		}
	}
	return nil
}

func isHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, m := s[:2], s[3:]
	if h < "00" || h > "23" {
		return false
	}
	if m < "00" || m > "59" {
		return false
	}
	return true
}

// MarshalLegs serialises legs into the JSONB column format.
func MarshalLegs(legs []Leg) ([]byte, error) {
	if legs == nil {
		legs = []Leg{}
	}
	return json.Marshal(legs)
}

// UnmarshalLegs is the inverse. Tolerates extra fields (forward
// compatibility per ADR-0012 D6).
func UnmarshalLegs(raw []byte) ([]Leg, error) {
	var out []Leg
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Backtest types — kept in sync with the FE result shape
// ─────────────────────────────────────────────────────────────────────────

// BacktestRequest is the body of POST /kairos/backtest.
type BacktestRequest struct {
	Name       string  `json:"name"`
	Underlying string  `json:"underlying"`
	Legs       []Leg   `json:"legs"`
	FromDate   string  `json:"fromDate"` // YYYY-MM-DD
	ToDate     string  `json:"toDate"`
	EntryTime  string  `json:"entryTime"`
	ExitTime   string  `json:"exitTime"`
	StopLoss   float64 `json:"stopLoss"`
	Target     float64 `json:"target"`
}

// Backtest is the persisted job row. Status moves
// pending → running → done|failed per ADR-0010 D4.
type Backtest struct {
	ID          uuid.UUID     `json:"id"`
	UserID      uuid.UUID     `json:"-"`
	StrategyID  *uuid.UUID    `json:"strategyId,omitempty"`
	Name        string        `json:"name"`
	Underlying  string        `json:"underlying"`
	Legs        []Leg         `json:"legs"`
	FromDate    string        `json:"fromDate"`
	ToDate      string        `json:"toDate"`
	EntryTime   string        `json:"entryTime"`
	ExitTime    string        `json:"exitTime"`
	StopLoss    *float64      `json:"stopLoss,omitempty"`
	Target      *float64      `json:"target,omitempty"`
	Status      string        `json:"status"`
	Result      *BacktestResult `json:"result,omitempty"`
	Error       string        `json:"error,omitempty"`
	StartedAt   *time.Time    `json:"startedAt,omitempty"`
	FinishedAt  *time.Time    `json:"finishedAt,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
}

// BacktestResult is the JSONB payload written when status='done'.
// Same shape as kairos/app/strategies/page.tsx renders.
type BacktestResult struct {
	TotalPnL    float64 `json:"totalPnl"`
	WinRate     float64 `json:"winRate"`
	Sharpe      float64 `json:"sharpe"`
	MaxDrawdown float64 `json:"maxDrawdown"`
	Trades      []Trade `json:"trades"`
}

// Trade is one entry in the result's trades array.
type Trade struct {
	Date     string  `json:"date"`     // YYYY-MM-DD
	Entry    float64 `json:"entry"`    // total entry premium received/paid
	Exit     float64 `json:"exit"`
	GrossPnL float64 `json:"grossPnl"`
	Costs    float64 `json:"costs"`
	NetPnL   float64 `json:"netPnl"`
	Outcome  string  `json:"outcome"` // win | loss | flat
}

// ─────────────────────────────────────────────────────────────────────────
// Chain wire format
// ─────────────────────────────────────────────────────────────────────────

// EnrichedChainRow is a stored ChainRow plus the greeks computed at
// read time by internal/kairos/greeks (ADR-0009 D3 — greeks are never
// persisted; this is the only place they're attached).
//
// Delta is abs() at the enrichment layer because chain-data.ts renders
// both sides positive. IV is in percent (e.g. 18.42), matching the FE's
// display convention; the greeks package itself works in fractions.
type EnrichedChainRow struct {
	provider.ChainRow
	IV    float64 `json:"iv"`
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
}

// ChainResponse is the JSON envelope returned by GET /kairos/options/chain.
// Matches what kairos/app/options/chain-data.ts expects, plus the
// staleness field per ADR-0013 D4.
type ChainResponse struct {
	Underlying       provider.Underlying `json:"underlying"`
	Spot             float64             `json:"spot"`
	Expiry           string              `json:"expiry"`
	SnapshotTime     time.Time           `json:"snapshot_time"`
	StalenessSeconds int64               `json:"staleness_seconds"`
	Rows             []EnrichedChainRow  `json:"rows"`
}

// ─────────────────────────────────────────────────────────────────────────
// Intraday analytics types
// ─────────────────────────────────────────────────────────────────────────

// IntradayPoint is one ingest tick on the intraday analytics chart:
// PCR and max pain at that snapshot, plus spot for the overlay line.
type IntradayPoint struct {
	TS      time.Time `json:"ts"`
	PCR     float64   `json:"pcr"`
	MaxPain int       `json:"maxPain"`
	Spot    float64   `json:"spot"`
}

// DaySnapshot is one snapshot tick on a trading day, carrying only the
// strike-level fields intraday analytics need. Produced by
// Store.DaySnapshots, consumed by the IntradayAnalytics handler.
type DaySnapshot struct {
	TS   time.Time
	Spot float64
	Rows []DayChainRow
}

// DayChainRow is the per-(strike, side) slice of a DaySnapshot.
type DayChainRow struct {
	Strike     int
	OptionType provider.OptionType
	LTP        float64
	OI         int64
}

// ProviderStatus is returned by GET /kairos/provider/status. The FE uses
// it to decide whether to prompt the operator to refresh Kite token.
type ProviderStatus struct {
	Active   string `json:"active"`
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason,omitempty"`
	LoginURL string `json:"login_url,omitempty"`
}
