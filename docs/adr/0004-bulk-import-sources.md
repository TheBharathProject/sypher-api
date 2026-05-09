# ADR-004 — Bulk import: source-discriminated importers

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-05-09 |
| Decision-maker | Shubham |

## Context

Pegasus has shipped CSV-based application imports since Phase 1: upload a CSV with columns `company,role,…`, get rows in your tracker. People who already have applications elsewhere — typically LinkedIn's "My Jobs" export or a Naukri export — can hand-edit those CSVs to match the Pegasus schema, but most won't.

Phase 4 adds first-class import for the two big external CSVs (LinkedIn, Naukri) without forcing users to remap columns themselves. Future sources (Gmail label scrape, Indeed export, manual paste) plug into the same pattern.

## Forces

- The existing CSV path works today; we don't want to break it.
- Each external source uses different column names (`Company Name` vs `company`, `Job Title` vs `role`, etc.), so a single hardcoded mapping can't serve all of them.
- Source-specific quirks: LinkedIn dates are `MM/DD/YYYY`; Naukri uses ISO-like; some columns are entirely absent.
- Scraping (Gmail OAuth + label parse, LinkedIn API) is out of scope for this phase — the API contract should leave room for it without committing to it now.

## Decisions

### D1 — One `Importer` interface, one impl per source

```go
type Importer interface {
    Name() string
    Parse(r io.Reader) (rows []ApplicationInput, errs []string, err error)
}
```

`Parse` returns rows + per-row errors + a fatal error (e.g. file unreadable). Per-row errors don't abort the parse — bad rows surface to the user; good rows still ship.

`importers.For(source string) (Importer, error)` is a string→impl lookup. Adding a new source is one new file + one switch case.

### D2 — Discriminator via `?source=` query param

`POST /applications/import?source=linkedin` selects the LinkedIn importer. Default `source=csv` keeps Phase 1 callers unchanged. Same for `/import/preview`.

**Rejected:** discriminated JSON body (`{source: "linkedin", payload: ...}`). The current path is multipart with a `file` field; preserving that for every CSV-shaped source means the frontend uses one upload widget across tabs. JSON body would require either base64-encoding the file or using two endpoints.

### D3 — Column matching is permissive + case-insensitive

LinkedIn's column names have shifted twice in the last year (the export tool is unstable). Each importer maintains a list of accepted variants per output field, picks the first match. Worst case (no match for required columns), Parse returns a fatal error naming the missing column.

This is friction up front (handler maintains synonyms) for resilience down the road (one column rename doesn't take down the import flow).

### D4 — Missing optional columns are OK

`appliedAt`, `applyDeadline`, `salaryRange`, `notes` etc. are all optional. If LinkedIn's CSV doesn't include them, the application lands with empty values and the user fills in later. Required: `company` + `role`. Both must map to a column with non-empty values; rows missing either are reported as errors.

### D5 — Date parsing per importer; output is ISO date

LinkedIn dates are `MM/DD/YYYY`; Naukri uses `YYYY-MM-DD` already. Each importer normalises to `YYYY-MM-DD` (the same shape Pegasus stores). Bad dates become empty + a per-row warning, not a hard reject — the rest of the row is still useful.

### D6 — Gmail-paste is a stub for now

The Gmail tab in the UI shows "Coming soon" and a one-line caption. No backend code yet. Lifting this to v2 keeps Phase 4 small and lets us land the LinkedIn/Naukri win without taking on Gmail OAuth. When a user asks for it, the same `Importer` interface fits — the source string becomes `gmail_paste` or `gmail_oauth`.

## Consequences

**Positive:**
- New source = one file + one switch case. No conditional handler logic.
- Backward-compatible: existing CSV callers (none currently — this is internal tooling) keep working without a `source` param.
- Each importer is independently testable. Day-1 tests cover the column-matching + date-normalisation paths.

**Negative:**
- Column synonyms live in code; LinkedIn renaming `Job Title` to `Position` means a code-and-deploy. Acceptable: external CSV exports change rarely and the fix is a one-line addition.
- Per-row errors are an unstructured `[]string`. When a future source needs richer error metadata (line number, suggested fix), this evolves to `[]ImportError{Line, Field, Suggestion}` — additive, no breaking change.
