package kairos

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FundamentalsRow mirrors kairos.equity_fundamentals (migration 0027,
// spec §3.4/§3.7). JSON tags are camelCase and must match
// ApiFundamental in kairos/lib/kairos-api.ts exactly — the FE client
// file is the contract. Numeric ratio columns are nullable in the
// table (a CSV upload can omit any of them), hence *float64: nil
// marshals to JSON null, which the FE renders as "—".
type FundamentalsRow struct {
	Symbol      string    `json:"symbol"`
	Name        string    `json:"name"`
	Sector      string    `json:"sector"`
	MktCapCr    *float64  `json:"mktCapCr"`
	PE          *float64  `json:"pe"`
	ROCE        *float64  `json:"roce"`
	ROE         *float64  `json:"roe"`
	DE          *float64  `json:"de"`
	NpY1        *float64  `json:"npY1"`
	NpY2        *float64  `json:"npY2"`
	OPM         *float64  `json:"opm"`
	Cagr3Profit *float64  `json:"cagr3Profit"`
	Cagr3Sales  *float64  `json:"cagr3Sales"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// screenerPresetWhere maps a preset key to its WHERE clause. The
// semantics MUST mirror applyFilter in kairos/app/screener/
// sample-data.ts exactly:
//
//	turnaround:  npY1 < 0 && npY2 > 0
//	earnings:    npY1 > 0 && npY2 > 0 && (npY2-npY1)/abs(npY1) > 0.25
//	high-roce:   roce > 20
//	compounders: (cagr3P ?? 0) > 15 && (cagr3S ?? 0) > 10
//	value-picks: pe !== null && pe < 20 && roe > 15
//	debt-free:   de !== null && de < 0.5
//	default:     everything (applyFilter's default branch — unknown
//	             keys fall through to all, so unknown presets do too)
//
// SQL NULL comparisons are falsy, which matches the TS guards: rows
// missing a ratio simply don't qualify (except compounders, where the
// TS `?? 0` coalesce is mirrored explicitly).
//
// The earnings ratio is rewritten multiplicatively —
// np_y2 - np_y1 > 0.25 * ABS(np_y1) — because Postgres doesn't
// guarantee short-circuit evaluation of AND, so the division form
// could divide by zero before the np_y1 > 0 guard runs. With
// np_y1 > 0 required, ABS(np_y1) > 0 and the forms are equivalent.
var screenerPresetWhere = map[string]string{
	"all":         "",
	"turnaround":  "np_y1 < 0 AND np_y2 > 0",
	"earnings":    "np_y1 > 0 AND np_y2 > 0 AND np_y2 - np_y1 > 0.25 * ABS(np_y1)",
	"high-roce":   "roce > 20",
	"compounders": "COALESCE(cagr3_profit, 0) > 15 AND COALESCE(cagr3_sales, 0) > 10",
	"value-picks": "pe IS NOT NULL AND pe < 20 AND roe > 15",
	"debt-free":   "de IS NOT NULL AND de < 0.5",
}

// screenerSortCols whitelists sort params onto real columns — the
// only way user input reaches the ORDER BY. Keys cover the
// ApiFundamental field names plus the sample-data.ts aliases the FE
// table headers still use (mktCap, cagr3P, cagr3S). Anything else
// falls back to market cap.
var screenerSortCols = map[string]string{
	"symbol":      "symbol",
	"name":        "name",
	"sector":      "sector",
	"mktCapCr":    "mkt_cap_cr",
	"mktCap":      "mkt_cap_cr",
	"pe":          "pe",
	"roce":        "roce",
	"roe":         "roe",
	"de":          "de",
	"npY1":        "np_y1",
	"npY2":        "np_y2",
	"opm":         "opm",
	"cagr3Profit": "cagr3_profit",
	"cagr3P":      "cagr3_profit",
	"cagr3Sales":  "cagr3_sales",
	"cagr3S":      "cagr3_sales",
	"updatedAt":   "updated_at",
}

const fundamentalsCols = `
	symbol, COALESCE(name, ''), COALESCE(sector, ''),
	mkt_cap_cr, pe, roce, roe, de, np_y1, np_y2, opm,
	cagr3_profit, cagr3_sales, updated_at`

// ListFundamentals returns the screener rows for a preset, sorted.
// preset / sortCol / dir are raw query params — both are resolved
// through the package maps above (never interpolated), so any value
// is safe. Unknown preset → all rows (mirrors applyFilter's default
// branch); unknown sortCol → market cap; dir is "asc" or anything →
// "desc" (the FE's default direction). NULLS LAST in both directions
// mirrors the FE comparator, which pins nulls to the bottom
// regardless of direction.
func (s *Store) ListFundamentals(ctx context.Context, preset, sortCol, dir string) ([]FundamentalsRow, error) {
	where, ok := screenerPresetWhere[preset]
	if !ok {
		where = ""
	}
	col, ok := screenerSortCols[sortCol]
	if !ok {
		col = "mkt_cap_cr"
	}
	order := "DESC"
	if dir == "asc" {
		order = "ASC"
	}
	q := "SELECT " + fundamentalsCols + " FROM kairos.equity_fundamentals"
	if where != "" {
		q += " WHERE " + where
	}
	q += fmt.Sprintf(" ORDER BY %s %s NULLS LAST, symbol ASC", col, order)

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FundamentalsRow{}
	for rows.Next() {
		var f FundamentalsRow
		if err := scanFundamental(rows, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFundamental returns one symbol's row. pgx.ErrNoRows passes
// through for the handler's 404.
func (s *Store) GetFundamental(ctx context.Context, symbol string) (*FundamentalsRow, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+fundamentalsCols+" FROM kairos.equity_fundamentals WHERE symbol = $1",
		symbol)
	var f FundamentalsRow
	if err := scanFundamental(row, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func scanFundamental(row pgx.Row, f *FundamentalsRow) error {
	return row.Scan(&f.Symbol, &f.Name, &f.Sector,
		&f.MktCapCr, &f.PE, &f.ROCE, &f.ROE, &f.DE,
		&f.NpY1, &f.NpY2, &f.OPM,
		&f.Cagr3Profit, &f.Cagr3Sales, &f.UpdatedAt)
}

// UpsertFundamentals writes rows into kairos.equity_fundamentals,
// keyed on symbol (ON CONFLICT DO UPDATE — re-uploading a CSV
// refreshes ratios in place). Returns the number of rows written.
// Batched in one round-trip; any failure aborts the batch and
// reports how many statements landed before it.
func (s *Store) UpsertFundamentals(ctx context.Context, rows []FundamentalsRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	b := &pgx.Batch{}
	for _, f := range rows {
		b.Queue(`
			INSERT INTO kairos.equity_fundamentals
				(symbol, name, sector, mkt_cap_cr, pe, roce, roe, de,
				 np_y1, np_y2, opm, cagr3_profit, cagr3_sales, updated_at)
			VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7, $8,
			        $9, $10, $11, $12, $13, NOW())
			ON CONFLICT (symbol) DO UPDATE SET
				name         = EXCLUDED.name,
				sector       = EXCLUDED.sector,
				mkt_cap_cr   = EXCLUDED.mkt_cap_cr,
				pe           = EXCLUDED.pe,
				roce         = EXCLUDED.roce,
				roe          = EXCLUDED.roe,
				de           = EXCLUDED.de,
				np_y1        = EXCLUDED.np_y1,
				np_y2        = EXCLUDED.np_y2,
				opm          = EXCLUDED.opm,
				cagr3_profit = EXCLUDED.cagr3_profit,
				cagr3_sales  = EXCLUDED.cagr3_sales,
				updated_at   = NOW()
		`, f.Symbol, f.Name, f.Sector, f.MktCapCr, f.PE, f.ROCE, f.ROE, f.DE,
			f.NpY1, f.NpY2, f.OPM, f.Cagr3Profit, f.Cagr3Sales)
	}
	br := s.pool.SendBatch(ctx, b)
	defer br.Close()
	n := 0
	for range rows {
		if _, err := br.Exec(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ListAnnouncements returns announcements newer than since, newest
// first, optionally filtered by category (case-insensitive exact
// match) and symbol (exact). limit defaults to 200 and is capped
// there too — that's the FE feed's page size.
func (s *Store) ListAnnouncements(ctx context.Context, since time.Time, category, symbol string, limit int) ([]Announcement, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(nse_id, ''), COALESCE(symbol, ''),
		       COALESCE(company, ''), COALESCE(category, ''),
		       COALESCE(headline, ''), COALESCE(attachment_url, ''),
		       announced_at
		FROM kairos.announcements
		WHERE announced_at >= $1
		  AND ($2 = '' OR LOWER(category) = LOWER($2))
		  AND ($3 = '' OR symbol = $3)
		ORDER BY announced_at DESC, id DESC
		LIMIT $4
	`, since, category, symbol, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Announcement{}
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.NseID, &a.Symbol, &a.Company,
			&a.Category, &a.Headline, &a.AttachmentURL, &a.AnnouncedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertAnnouncements inserts rows, deduped on nse_id
// (ON CONFLICT DO NOTHING — the 10-min cron re-fetches the same feed
// all day, so most rows are repeats). Returns how many rows were
// actually new. Raw (the source payload) lands in the jsonb column;
// it never leaves the database through the API.
func (s *Store) InsertAnnouncements(ctx context.Context, rows []Announcement) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	b := &pgx.Batch{}
	for _, a := range rows {
		b.Queue(`
			INSERT INTO kairos.announcements
				(nse_id, symbol, company, category, headline,
				 attachment_url, announced_at, raw)
			VALUES (NULLIF($1, ''), NULLIF($2, ''), NULLIF($3, ''),
			        NULLIF($4, ''), $5, NULLIF($6, ''), $7, $8)
			ON CONFLICT (nse_id) DO NOTHING
		`, a.NseID, a.Symbol, a.Company, a.Category, a.Headline,
			a.AttachmentURL, a.AnnouncedAt, a.Raw)
	}
	br := s.pool.SendBatch(ctx, b)
	defer br.Close()
	inserted := 0
	for i := range rows {
		tag, err := br.Exec()
		if err != nil {
			return inserted, fmt.Errorf("announcement %d/%d: %w", i+1, len(rows), err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}
