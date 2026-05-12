# CTO Decision Index

## Cross-Cutting Principles

1. Business case first — every recommendation must tie to a measurable outcome or a concrete risk reduction.
2. One-way doors (platform, DB schema, public API contracts) get full analysis with alternatives. Two-way doors get a paragraph.
3. YAGNI: flag over-engineering for hypothetical scale. Pegasus is a single-pod OCI deployment; design for that, not for 100x.
4. Dependency discipline: each new runtime dependency must justify its weight. Zero-dep defaults are preferred when a custom solution costs less than 1 engineer-day.
5. Test coverage gates features: new code paths in billing, auth, and AI credit deduction require at minimum a unit test before merge.
6. Observability is not optional past ~100 DAU: `slog` is sufficient now; add request-ID propagation before adding a second OCI instance.

---

## Feature Decisions

- `pegasus-gap-analysis` — CTO-level gap analysis: architectural inventory of completed phases, build/buy for PDF gen and focus trap, risk assessment, sequencing for Phases 10/12/14/15 — Accepted (2026-05-12)
