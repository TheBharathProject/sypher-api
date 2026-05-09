# sypher-api — Contributor's Guide

This is the canonical reference for adding ANYTHING to `sypher-api`: a new endpoint, a new tool's API, a new auth method, a new shared utility. Read it once cover-to-cover; come back as a lookup.

It's also a Go-on-sypher tutorial — if you're new to Go, the patterns explained here are exactly what you'll see in the codebase. Pair it with [A Tour of Go](https://go.dev/tour) and [Effective Go](https://go.dev/doc/effective_go) for the language itself.

## Contents

1. [What this service is](#1-what-this-service-is)
2. [Architecture at a glance](#2-architecture-at-a-glance)
3. [Repository tour](#3-repository-tour)
4. [The tool package pattern](#4-the-tool-package-pattern)
5. [Shared packages — when to use each](#5-shared-packages--when-to-use-each)
6. [Adding a new endpoint to an existing tool](#6-adding-a-new-endpoint-to-an-existing-tool)
7. [Adding a new tool from scratch](#7-adding-a-new-tool-from-scratch)
8. [Database access](#8-database-access)
9. [Configuration](#9-configuration)
10. [HTTP plumbing](#10-http-plumbing)
11. [Authentication](#11-authentication)
12. [Caching](#12-caching)
13. [Logging](#13-logging)
14. [Errors](#14-errors)
15. [Testing](#15-testing)
16. [The rules — never violate these](#16-the-rules--never-violate-these)
17. [Deploy lifecycle](#17-deploy-lifecycle)
18. [Local development](#18-local-development)
19. [Operational cheatsheet](#19-operational-cheatsheet)
20. [Go idioms reference](#20-go-idioms-reference)

**Appendices**

- [A. Reel Hooks — Tier 2 worked example](#appendix-a-reel-hooks--tier-2-worked-example)

---

## 1. What this service is

`sypher-api` is a **modular monolith**: one Go binary, one Docker image, one URL (`api.sypher.in`), serving every backend endpoint sypher needs.

Why a monolith and not microservices:

- **One founder, indie scale.** Microservices solve org problems (multiple teams owning bounded contexts), not technical ones at this size.
- **Shared concerns are free.** Auth, billing, user identity — all in-process function calls, not HTTP between services.
- **One DB pool.** Microservices would each open their own connection pool to the same Postgres. Wasteful.
- **One deploy unit.** A code change ships in one CI run. A schema change applies once.

The discipline: **separation in the codebase, unification at runtime.** Each tool is its own package under `internal/<tool>/`; they can't see each other except through `internal/platform/*` shared APIs. Logical isolation, no operational tax.

If a single tool ever genuinely needs to scale or fail differently — extract it then. That decision should be forced by data, not anticipated.

---

## 2. Architecture at a glance

```
                       https://api.sypher.in
                              │
                              ▼
                     ┌──────────────────┐
                     │ Caddy on the VM  │  reverse proxy, TLS, vhost routing
                     └────────┬─────────┘
                              │
                              ▼  http://127.0.0.1:8002
                     ┌──────────────────┐
                     │ sypher-api       │  Go binary, ~14 MB
                     │ (Docker)         │  listens on 0.0.0.0:8000 inside
                     └────────┬─────────┘
                              │
                              ▼  pgxpool over postgres:5432  (sypher-net Docker network)
                     ┌──────────────────┐
                     │ sypher-postgres  │  Postgres 18 (Docker)
                     │ (Docker)         │  data in ~/pg-data
                     └──────────────────┘
```

A request:

1. Browser → `https://sypher.in/api/waitlist` (Vercel Next.js route)
2. Next.js route validates + forwards to `https://api.sypher.in/waitlist` with shared API key
3. Caddy terminates TLS, proxies to `127.0.0.1:8002`
4. Go binary's middleware chain runs (recover → log → CORS)
5. Mux dispatches to the waitlist handler
6. Handler validates body, calls store
7. Store runs SQL via `pgxpool`
8. JSON response bubbles back up, logged on the way out

Every step is observable: structured JSON logs at every layer.

---

## 3. Repository tour

```
sypher-api/
├── cmd/
│   ├── api/main.go              The server binary entry. Boots config → db pool →
│   │                            server → traps SIGTERM. ~50 lines.
│   └── migrate/main.go          One-shot migration runner. Walks migrations/ in
│                                lex order, applies each .sql file. ~75 lines.
│
├── internal/
│   ├── config/                  Env-var parsing. Fail-fast on missing.
│   ├── db/                      pgxpool factory. Sized small (1–4 conns).
│   ├── server/                  HTTP plumbing: mux, middleware, lifecycle.
│   ├── httpx/                   SHARED HTTP utilities — JSON writers, ClientIP.
│   ├── security/                SHARED security utilities — HashIP, future hashing.
│   ├── health/                  GET /health endpoint.
│   └── waitlist/                THE FIRST TOOL — types, store, handler.
│       (each future tool follows this exact 3-file shape)
│
├── migrations/
│   └── 0001_waitlist.sql        Idempotent SQL. Never edit existing files; add new ones.
│
├── scripts/
│   └── deploy.sh                Operator script — runs on the VM. Pulls image,
│                                runs migrate one-shot, replaces container, probes /health.
│
├── .github/workflows/ci.yml     Lint + build + test on PR; multi-arch GHCR publish on main.
├── docs/GUIDE.md                ← you are here
├── Dockerfile                   Multi-stage. Final image ~48 MB on alpine.
├── entrypoint.sh                Branches on first arg: serve | migrate | <anything else>
├── go.mod / go.sum              Module declaration + lock file. One real dep: pgx/v5.
├── .dockerignore / .env.example / .gitignore
└── README.md                    Quickstart + deploy. The "what is this and how do I run it" page.
```

The mental model: **`cmd/` is entry points, `internal/` is everything else, `internal/<tool>/` is one tool's whole world.**

The `internal/` prefix is a Go feature: anything under it can ONLY be imported by code in the same module. External packages can never depend on these — keeps our internals truly internal.

---

## 4. The tool package pattern

Every tool's API is a single package under `internal/<tool>/` containing exactly **three files**:

| File | Owns |
|---|---|
| `types.go` | Request and response struct shapes, validation methods, sentinel errors |
| `store.go` | Database access — SQL queries and how rows turn into Go structs |
| `handler.go` | HTTP layer — parses body, calls validation, calls store, writes response |

Walking through `internal/waitlist/` as the canonical example:

### `types.go` — the contract

```go
type Request struct {
    Email    string `json:"email"`
    Source   string `json:"source,omitempty"`
    Referrer string `json:"referrer,omitempty"`
    HP       string `json:"hp,omitempty"` // honeypot
}

type Response struct {
    OK  bool `json:"ok"`
    New bool `json:"new"`
}

var ErrInvalidEmail = errors.New("invalid email")

func (r *Request) Normalize() { /* lowercase + trim + truncate optional fields */ }
func (r *Request) Validate() error { /* returns ErrInvalidEmail or nil */ }
```

The struct field tags (\`json:"email"\`) are **mandatory** — Go's default JSON decoder uses the exported field names, which would expect `{"Email": ...}`. Tags map our internal naming to wire JSON.

`omitempty` on a *response* field skips it when zero-valued. On *request* fields it's a no-op; including it documents that the field is optional.

`Validate()` is a method on the type, not a free function. The type owns its own correctness — handlers never re-implement validation rules.

### `store.go` — the database layer

```go
type Store struct {
    pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
    return &Store{pool: pool}
}

func (s *Store) Insert(ctx context.Context, email, source, ...) (bool, error) {
    const q = `INSERT INTO waitlist.signups (...) VALUES ($1, $2, ...)
               ON CONFLICT (lower(email)) DO NOTHING RETURNING id`
    var id int64
    err := s.pool.QueryRow(ctx, q, email, source, ...).Scan(&id)
    switch {
    case err == nil:                       return true, nil
    case errors.Is(err, pgx.ErrNoRows):    return false, nil   // duplicate
    default:                                return false, err
    }
}
```

Things to notice:

- **`context.Context` is the first parameter.** Every function that does I/O must accept one. It carries the deadline (`http.Request.Context()`) so a slow query gets cancelled when the client disconnects.
- **Raw SQL.** No ORM. `pgxpool` lets you write the SQL you'd run in psql.
- **Lowercased email at the SQL level** (`lower($1)`). The store owns the canonical row shape; handlers can't accidentally insert `"User@Example.com"`.
- **Switch on error type.** `pgx.ErrNoRows` is a sentinel — `errors.Is` walks the wrap chain.
- **Returns `(bool, error)`**, not `(*Result, error)`. The boolean is the meaningful answer; nothing else needs to come back.

### `handler.go` — the HTTP layer

```go
type Handler struct {
    store  *Store
    salt   string
    logger *slog.Logger
}

func NewHandler(store *Store, ipSalt string, logger *slog.Logger) *Handler {
    return &Handler{store: store, salt: ipSalt, logger: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // 1. Limit body size
    body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024))
    // 2. Decode JSON
    var req Request
    json.Unmarshal(body, &req)
    // 3. Honeypot — pretend success silently
    if req.HP != "" { ... }
    // 4. Normalize + validate via the type's own methods
    req.Normalize()
    if err := req.Validate(); err != nil { ... }
    // 5. Build the side-effects (IP hash, user agent)
    ipHash := security.HashIP(h.salt, httpx.ClientIP(r))
    // 6. Call the store
    created, err := h.store.Insert(r.Context(), req.Email, ...)
    // 7. Respond
    httpx.WriteJSON(w, http.StatusOK, Response{OK: true, New: created})
}

var _ http.Handler = (*Handler)(nil)  // compile-time interface assertion
```

The handler is **small by design**. It does NOT contain validation logic, hashing logic, or SQL. Each of those lives in its proper home.

The trailing `var _ http.Handler = (*Handler)(nil)` is a **compile-time assertion** that `*Handler` satisfies the `http.Handler` interface. If we ever break the interface (rename `ServeHTTP`, change its signature), the build fails immediately rather than at request time.

### What does NOT go in a tool package

- HTTP utilities like `WriteJSON`, `ClientIP` — those live in `internal/httpx`
- Hashing, salting — `internal/security`
- Cross-tool helpers — `internal/platform/*` (we'll add this when we have one)
- DB pool initialization — `internal/db`

### Scaling beyond 3 files — when one tool has many endpoints

The three-file pattern (`types.go` / `store.go` / `handler.go`) is the **starting** shape, optimal for ~1–5 endpoints. Real tools grow.  Reel Hooks will probably hit 8–12 endpoints; viral-typography-creator's existing API has **55+ endpoints across 16 resource groups**. Forcing 55 handlers into one `handler.go` would be unreadable.

The mental model: as the tool grows, **split by resource, not by layer**. A single `handler.go` with 55 functions is bad; an equivalent `handlers/` folder with 16 files (`projects.go`, `audio.go`, `users.go`, ...) where each file owns *all* of one resource's handlers is good.

There are three sizes a tool can be at, and the layout follows. Pick the layout that matches your expected endpoint count from day one — refactoring up a tier is fine, but doing it for every tool is wasted churn.

#### Tier 1 — Small (1–5 endpoints) → 3 files, current shape

```
internal/waitlist/
├── types.go
├── store.go
└── handler.go
```

This is `waitlist` today. One `Handler` struct, one or two methods, one `Store` with a few queries.

#### Tier 2 — Medium (5–20 endpoints) → split by layer into folders

When `handler.go` has more than ~5 methods, split it. Same with `store.go`.

```
internal/reelhooks/
├── routes.go              ← single source of truth — mounts every endpoint
├── types.go               ← shared DTOs (Profile, Reel, etc.)
├── handlers/
│   ├── profiles.go        ← all /reel-hooks/profiles* endpoints
│   ├── reels.go           ← all /reel-hooks/reels* endpoints
│   └── analyses.go
└── store/
    ├── profiles.go
    ├── reels.go
    └── analyses.go
```

Each `handlers/<resource>.go` owns ALL the handler funcs for that resource (List, Get, Create, Update, Delete). Same shape on the store side.

`routes.go` becomes the table-of-contents for the tool — one place to grep when you ask "what endpoints does this tool expose?":

```go
// internal/reelhooks/routes.go
package reelhooks

import (
    "net/http"

    "github.com/jackc/pgx/v5/pgxpool"
    "log/slog"

    "github.com/TheBharathProject/sypher-api/internal/reelhooks/handlers"
    "github.com/TheBharathProject/sypher-api/internal/reelhooks/store"
)

type Module struct {
    profiles *handlers.Profiles
    reels    *handlers.Reels
    analyses *handlers.Analyses
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Module {
    profileStore := store.NewProfiles(pool)
    reelStore    := store.NewReels(pool)
    analysisStore := store.NewAnalyses(pool)

    return &Module{
        profiles: handlers.NewProfiles(profileStore, logger),
        reels:    handlers.NewReels(reelStore, logger),
        analyses: handlers.NewAnalyses(analysisStore, logger),
    }
}

// RegisterRoutes mounts every endpoint this tool exposes.
// Caller passes any middleware they want applied to all of these.
func (m *Module) RegisterRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
    // Profiles
    mux.Handle("GET    /reel-hooks/profiles",        auth(http.HandlerFunc(m.profiles.List)))
    mux.Handle("POST   /reel-hooks/profiles",        auth(http.HandlerFunc(m.profiles.Create)))
    mux.Handle("GET    /reel-hooks/profiles/{id}",   auth(http.HandlerFunc(m.profiles.Get)))
    mux.Handle("DELETE /reel-hooks/profiles/{id}",   auth(http.HandlerFunc(m.profiles.Delete)))

    // Reels
    mux.Handle("GET    /reel-hooks/profiles/{id}/reels", auth(http.HandlerFunc(m.reels.ListByProfile)))
    mux.Handle("GET    /reel-hooks/reels/{id}",          auth(http.HandlerFunc(m.reels.Get)))

    // Analyses
    mux.Handle("GET    /reel-hooks/analyses/{id}",       auth(http.HandlerFunc(m.analyses.Get)))
    mux.Handle("POST   /reel-hooks/analyses",            auth(http.HandlerFunc(m.analyses.Create)))
}
```

Then `internal/server/server.go` shrinks to:

```go
// In routes()
rh := reelhooks.New(s.pool, s.logger)
rh.RegisterRoutes(mux, requireAuth)
```

server.go stays small no matter how many endpoints the tool has.

**This is the right starting point for any tool with auth + CRUD on multiple resources.** Reel Hooks will start here.

#### Tier 3 — Large (20+ endpoints) → sub-packages per resource

When a single resource's handler/store file passes ~300 lines, that resource gets its own sub-package.

```
internal/typography/
├── routes.go              ← still the single source of truth
├── module.go              ← Module struct, New(), wiring
├── projects/              ← each resource is its own package
│   ├── handler.go
│   ├── store.go
│   ├── types.go
│   └── transformer.go     ← anything else this resource needs
├── audio/
│   ├── handler.go
│   ├── store.go
│   ├── types.go
│   └── upload.go
├── users/
│   ├── handler.go
│   ├── store.go
│   └── types.go
├── editor/
│   └── ...
├── style_packs/
│   └── ...
└── shared/                ← types/utilities used across multiple resources
    ├── types.go           ← e.g. PaginatedRequest, common error envelopes
    └── transforms.go
```

This is what `viral-typography-creator/apps/api` would look like if rewritten in Go. Each `<resource>/` package follows the Tier-1 three-file pattern internally — recursion of the same shape at a smaller scope.

`routes.go` and `module.go` at the top level still own registration; they import each sub-package and call into it.

**Why sub-packages and not just more folders inside `handlers/`:** Go's package boundaries enforce isolation. With sub-packages, the `users` package can't accidentally reach into `projects` internals — only through exported types. With deeper folders inside one package, everything's still in one namespace and the discipline depends on you remembering. Sub-packages make the rule a compile error.

### Choosing your starting tier

| Expected endpoints | Tier | When |
|---|---|---|
| 1–5 | 1 — three files | Marketing endpoints, webhooks, single-resource tools (waitlist) |
| 5–20 | 2 — handlers/ + store/ folders | Most "real" tools — Reel Hooks, hashtag research, simple analytics |
| 20+ | 3 — sub-packages per resource | Full-product APIs — typography editor, market data lab, multi-domain admin |

**Rule of thumb:** if you expect ≥10 endpoints from day one, start at Tier 2. Refactoring Tier-1 → Tier-2 is mechanical (rename files, split a function), but doing it for tool #1, #2, #3 is wasted effort. Tier-2 is also fine for 5-endpoint tools that you expect will grow.

### The discipline that holds across all tiers

1. **`RegisterRoutes` is the canonical surface.** No matter how many files the tool has internally, there's exactly **one function** that mounts every endpoint. Grep `RegisterRoutes` to inventory the API.
2. **Tools own their schema.** Tier doesn't change this — even a 50-endpoint tool keeps its tables in `<tool>` schema, isolated from siblings.
3. **No tool imports another tool.** Whether monolithic or split into 10 sub-packages — `internal/reelhooks/*` can never import `internal/markets/*`. Shared concerns lift to `internal/platform/*`.
4. **Sub-package depth maxes at 2.** `internal/typography/projects/` is fine. `internal/typography/projects/v2/` is a smell — that's a code change, not a folder change.
5. **Split by resource via folders, not by file suffix.** When you outgrow Tier 1, the upgrade path is `handlers/profiles.go`, NOT `profiles_handler.go` flat in the package root. Folders give a real namespace (`handlers.NewProfiles()` reads better than `reelhooks.NewProfilesHandler()`) and they compose cleanly into Tier 3 if the tool keeps growing. Both styles compile; only the folder style is canonical for sypher.

---

## 5. Shared packages — when to use each

| Package | Purpose | Add to it when |
|---|---|---|
| `internal/config` | Env-var loading | You need a new config knob |
| `internal/db` | Pool initialization | Almost never — already done |
| `internal/httpx` | HTTP utilities (`WriteJSON`, `WriteError`, `ClientIP`, `ErrorBody`) | You're tempted to write a JSON helper inside a tool |
| `internal/security` | Hashing, salting, anything crypto-adjacent | You need to hash a different identifier (email? phone? user ID?) |
| `internal/server` | HTTP server lifecycle, middleware, route registration | You have a new cross-cutting middleware (rate limit, request ID, etc.) |

When you find yourself writing the same helper in two tools, that's the signal to lift it into a shared package — typically `httpx` or a new `internal/platform/<concern>/`.

The discipline:

- A **tool package** can import `internal/httpx`, `internal/security`, `internal/db`, and (in the future) `internal/platform/*`
- A **tool package CAN NEVER import another tool package.** `reelhooks` cannot import `markets`. If you find yourself wanting to, the shared concern goes into `internal/platform/`.

---

## 6. Adding a new endpoint to an existing tool

Say you want to add `GET /waitlist/count` to expose how many signups exist.

### Step 1 — Add the store method (`internal/waitlist/store.go`)

```go
func (s *Store) Count(ctx context.Context) (int64, error) {
    var n int64
    err := s.pool.QueryRow(ctx, "SELECT count(*) FROM waitlist.signups").Scan(&n)
    return n, err
}
```

### Step 2 — Add a response type (`internal/waitlist/types.go`)

```go
type CountResponse struct {
    Count int64 `json:"count"`
}
```

### Step 3 — Add the handler (`internal/waitlist/handler.go`)

```go
func (h *Handler) HandleCount(w http.ResponseWriter, r *http.Request) {
    n, err := h.store.Count(r.Context())
    if err != nil {
        h.logger.Error("waitlist count", "err", err)
        httpx.WriteError(w, http.StatusInternalServerError, "db_error", "could not fetch count")
        return
    }
    httpx.WriteJSON(w, http.StatusOK, CountResponse{Count: n})
}
```

### Step 4 — Wire the route (`internal/server/server.go`)

```go
mux.Handle("POST /waitlist", apiKey(wlHandler))
// new:
mux.HandleFunc("GET /waitlist/count", wlHandler.HandleCount)
```

Decide whether the new route should be auth-gated. If yes, wrap it: `mux.Handle("GET /waitlist/count", apiKey(http.HandlerFunc(wlHandler.HandleCount)))`.

### Step 5 — Test locally, then push

```bash
go run ./cmd/api &
curl http://127.0.0.1:8000/waitlist/count
```

Push when green. CI builds, GHCR publishes, run `bash deploy.sh` on the VM.

---

## 7. Adding a new tool from scratch

Say you're building Reel Hooks — a new tool with profile-tracking endpoints.

### Step 0 — Pick the right starting tier

Before creating files, look at [§4 → Scaling beyond 3 files](#scaling-beyond-3-files--when-one-tool-has-many-endpoints) and decide:

- **≤5 endpoints expected:** Tier 1 (three flat files). Keep reading this section as written.
- **5–20 endpoints expected:** Tier 2 (`handlers/` + `store/` folders + `routes.go`). The walkthrough below still applies — just lay out the folders and `RegisterRoutes` per Tier 2 from day one.
- **20+ endpoints expected:** Tier 3 (sub-packages per resource). Same walkthrough, but each resource is its own package.

**Reel Hooks expects ~10 endpoints, so it starts at Tier 2.** The example below uses Tier 1's flat layout for clarity; mentally substitute the Tier-2 folders if your tool fits there.

### Step 1 — Create the package directory

```bash
# Tier 1 — flat
mkdir -p internal/reelhooks
touch internal/reelhooks/{types,store,handler}.go

# Tier 2 — folders (use this if you expect ≥5 endpoints)
mkdir -p internal/reelhooks/{handlers,store}
touch internal/reelhooks/{routes.go,types.go}
touch internal/reelhooks/handlers/profiles.go
touch internal/reelhooks/store/profiles.go
```

### Step 2 — Write the migration (`migrations/0002_reelhooks.sql`)

```sql
CREATE SCHEMA IF NOT EXISTS reel_hooks;

CREATE TABLE IF NOT EXISTS reel_hooks.tracked_profiles (
  id          BIGSERIAL PRIMARY KEY,
  user_id     UUID NOT NULL,
  handle      TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (user_id, lower(handle))
);
```

**Migration rules:**

- File name format: `NNNN_<tool-or-feature>.sql` — numbers match deploy order
- Always idempotent (`CREATE ... IF NOT EXISTS`, `ALTER ... ADD COLUMN IF NOT EXISTS`)
- **Never edit a file that's already been deployed** — write a new one (`0003_*.sql`) that alters
- Each tool owns its own schema (`reel_hooks.*`, `markets.*`) — keeps the public schema clean

### Step 3 — Build the package

Mirror the waitlist three-file shape: `types.go` (Request/Response/Validate), `store.go` (Insert/List/Get/etc.), `handler.go` (Handler struct with NewHandler constructor and method handlers).

### Step 4 — Mount routes (`internal/server/server.go`)

**Tier 1 (flat) — direct mount:**

```go
import "github.com/TheBharathProject/sypher-api/internal/reelhooks"

func (s *Server) routes() http.Handler {
    mux := http.NewServeMux()

    mux.HandleFunc("GET /", s.handleRoot)
    mux.Handle("GET /health", health.NewHandler(s.pool))

    // Waitlist
    apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
    wlHandler := waitlist.NewHandler(waitlist.NewStore(s.pool), s.cfg.IPSalt, s.logger)
    mux.Handle("POST /waitlist", apiKey(wlHandler))

    // Reel Hooks (Tier 1 — only if very few endpoints)
    rhHandler := reelhooks.NewHandler(reelhooks.NewStore(s.pool), s.logger)
    mux.Handle("GET /reel-hooks/profiles", apiKey(http.HandlerFunc(rhHandler.HandleList)))
    mux.Handle("POST /reel-hooks/profiles", apiKey(http.HandlerFunc(rhHandler.HandleCreate)))

    // ... middleware ...
    return h
}
```

**Tier 2 / Tier 3 — delegate to the tool's `RegisterRoutes`:**

```go
// In server.go's routes()
mux := http.NewServeMux()
mux.HandleFunc("GET /", s.handleRoot)
mux.Handle("GET /health", health.NewHandler(s.pool))
apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
mux.Handle("POST /waitlist", apiKey(waitlist.NewHandler(waitlist.NewStore(s.pool), s.cfg.IPSalt, s.logger)))

// Reel Hooks — every endpoint registered in one call
rh := reelhooks.New(s.pool, s.logger)
rh.RegisterRoutes(mux, apiKey)

// ... middleware ...
```

The point of `RegisterRoutes`: server.go stays the same size whether the tool has 3 or 100 endpoints. All the route mounts live inside the tool's package, where they belong.

### Step 5 — Add tool-specific config if needed

If the tool needs new env vars (e.g. `APIFY_API_TOKEN`), add to `internal/config/config.go`:

```go
type Config struct {
    // ... existing
    ApifyAPIToken string
}

cfg := &Config{
    // ... existing
    ApifyAPIToken: required("APIFY_API_TOKEN"),
}
```

Update `.env.example`. Update Vercel env vars + `~/.pg-secret` on the VM.

### Step 6 — Push and deploy

GHA builds + publishes the image. On the VM: `bash ~/services/sypher-api/deploy.sh` runs the migration first (idempotently — no-op if you already applied it manually) and starts the new container.

---

## 8. Database access

### Pool, not raw connections

`pgxpool` is created once at startup in `cmd/api/main.go`. Every store gets the same `*pgxpool.Pool`. **Never `pgx.Connect()` per request** — that's a fresh TCP+TLS handshake each time.

### Always pass `context.Context`

```go
// Good
func (s *Store) Get(ctx context.Context, id int64) (*Foo, error) { ... }

// Bad — caller can't cancel; will block past the request lifetime
func (s *Store) Get(id int64) (*Foo, error) { ... }
```

In handlers, pass `r.Context()` down. Cancellation propagates: client disconnect → ctx cancel → query cancelled mid-flight.

### Single-row reads

```go
var name string
err := s.pool.QueryRow(ctx, "SELECT name FROM users WHERE id = $1", id).Scan(&name)
if errors.Is(err, pgx.ErrNoRows) {
    return nil, ErrNotFound
}
if err != nil {
    return nil, fmt.Errorf("query: %w", err)
}
```

### Multi-row reads

```go
rows, err := s.pool.Query(ctx, "SELECT id, name FROM users WHERE org = $1", orgID)
if err != nil {
    return nil, err
}
defer rows.Close()  // ALWAYS defer the Close on Query

var users []User
for rows.Next() {
    var u User
    if err := rows.Scan(&u.ID, &u.Name); err != nil {
        return nil, err
    }
    users = append(users, u)
}
return users, rows.Err()  // rows.Err() catches mid-stream failures
```

For struct mapping, prefer `pgx.CollectRows` + `pgx.RowToStructByName`:

```go
rows, _ := s.pool.Query(ctx, q, args...)
return pgx.CollectRows(rows, pgx.RowToStructByName[User])
```

### Writes — use `Exec` if no return; `QueryRow` if you need RETURNING

```go
// Plain INSERT, no return
ct, err := s.pool.Exec(ctx, "INSERT INTO ...")
rows := ct.RowsAffected()  // returns int64

// INSERT with RETURNING id
var id int64
err := s.pool.QueryRow(ctx, "INSERT INTO ... RETURNING id").Scan(&id)
```

### Transactions

When you need atomic multi-statement ops:

```go
tx, err := s.pool.Begin(ctx)
if err != nil { return err }
defer tx.Rollback(ctx)  // safe — Commit before this line makes Rollback a no-op

if _, err := tx.Exec(ctx, "..."); err != nil { return err }
if _, err := tx.Exec(ctx, "..."); err != nil { return err }

return tx.Commit(ctx)
```

The `defer tx.Rollback(ctx)` pattern is critical. If anything between `Begin` and `Commit` errors out (including `panic`s), the deferred rollback fires.

### Schema convention

| Concept | Schema |
|---|---|
| Cross-tool tables (users, subscriptions, audit_log) | `public` |
| Per-tool tables | `<tool_slug_underscored>` (e.g. `waitlist`, `reel_hooks`, `markets`) |

Each tool's migrations create + populate its own schema. Tools can read from `public` for shared concerns; they NEVER read from another tool's schema directly.

### Migrations

- Files: `migrations/NNNN_<thing>.sql`
- Always idempotent — re-running on an already-up DB must be a no-op
- One concern per file (don't bundle waitlist + reel-hooks into one migration)
- Run by the `migrate` binary, dispatched by `entrypoint.sh migrate`
- Applied automatically by `deploy.sh` before the new container starts

If you ever need to **roll back a schema change**, write a new forward migration. There's no down-migration support; this is a deliberate simplification — keep schema changes additive when possible (add column, soft-deprecate old, remove later in a separate migration).

---

## 9. Configuration

Every runtime knob is an env var, parsed once at startup in `internal/config/config.go`. The pattern: required vars cause a fail-fast, optional vars have defaults.

### Adding a new env var

Four places to update:

1. **`internal/config/config.go`** — add field to `Config` struct, add to `Load()`:
   ```go
   type Config struct {
       // ... existing
       OpenAIAPIKey string
   }

   cfg := &Config{
       // ... existing
       OpenAIAPIKey: required("OPENAI_API_KEY"),
   }
   ```

2. **`.env.example`** — add the var with a placeholder value and a comment explaining what it is.

3. **`~/.pg-secret` on the VM** — add the real value. Never commit the real value.

4. **`scripts/deploy.sh`** — add `-e OPENAI_API_KEY="$OPENAI_API_KEY"` to the `docker run` command.

Then update README's "first-time deployment" section to mention the new var.

### Why `required()` returns errors instead of `panic`

Failing fast is good; *crashing* is bad. The current pattern (`return nil, fmt.Errorf(...)`) lets `main()` log the error cleanly via `slog` and exit 1. A `panic` would print a stack trace nobody wants to read. Always prefer returning errors.

---

## 10. HTTP plumbing

### Routing

We use stdlib `net/http` with the Go 1.22+ method-aware mux:

```go
mux.HandleFunc("GET /health", handler)
mux.HandleFunc("POST /waitlist", handler)
mux.Handle("GET /users/{id}", handler)  // path parameter
```

Inside the handler, get path params via `r.PathValue("id")`.

We do NOT use `chi`, `gin`, `echo`, etc. The stdlib is enough for everything we need. If we ever need patterns the stdlib can't express (sub-router groups with shared middleware), we'll add `chi` — but not before.

### Middleware chain

In `internal/server/server.go`:

```go
var h http.Handler = mux
h = withCORS(h, s.cfg.CORSOrigins)
h = withLogging(h, s.logger)
h = withRecover(h, s.logger)
return h
```

Read bottom-up: incoming request hits `withRecover` first, then `withLogging`, then `withCORS`, then mux. Outgoing response unwinds in reverse.

**Rule:** `withRecover` is always outermost. It must catch panics from everything below it.

### Adding a new middleware

Middleware in Go is a function that wraps an `http.Handler`:

```go
func withRequestID(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        id := generateID()
        w.Header().Set("X-Request-ID", id)
        ctx := context.WithValue(r.Context(), "requestID", id)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

Mount it in `routes()`:

```go
h = withRequestID(h)
```

### Middleware factories

When middleware needs config, return a closure:

```go
func requireAPIKey(expected string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // ... compare r.Header.Get("X-API-Key") to expected
            next.ServeHTTP(w, r)
        })
    }
}

// Usage:
apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
mux.Handle("POST /waitlist", apiKey(handler))
```

This is the "configurable middleware" pattern. You'll see it again for rate limiting, role-based auth, etc.

### Response shape

Always return JSON, even for errors. Use `httpx.WriteJSON` and `httpx.WriteError`. Never call `w.Write([]byte(...))` directly with arbitrary text.

Error envelope is canonical:

```json
{ "error": "bad_email", "message": "invalid email" }
```

The `error` field is a stable machine-readable code. The `message` is human-friendly and may change wording.

---

## 11. Authentication

### Current state

One auth mechanism: **shared `X-API-Key` header**, used by `/waitlist`. The Vercel-side `/api/waitlist` route forwards this header from a server-only env var. Browsers never see the key.

This works because:
- The waitlist write path doesn't need user identity
- Only one trusted caller (Vercel) talks to the endpoint
- Key rotation is one env-var change on Vercel

### When you need real user auth

Triggers:
- An endpoint reads or writes per-user data (`/me`, `/projects/mine`)
- An endpoint authorises by subscription (`/reel-hooks/...` only for paying users)
- Multiple clients call it (browser directly, mobile app, etc.)

### Adding JWT-based auth

The pattern:

1. **Store auth state in Postgres.** Already partly designed in `sypher-shell/docs/sypher-factory/auth-and-billing.md` — Supabase auth issues JWTs, our service validates them.

2. **New shared package: `internal/platform/auth/`**

   ```
   internal/platform/auth/
   ├── jwt.go          ParseAndVerify(token, jwks) returns *Claims
   ├── middleware.go   requireAuth() — extracts JWT from Authorization header
   └── context.go      UserFromContext(ctx) — handlers read identity from ctx
   ```

3. **The auth middleware:**

   ```go
   func RequireAuth(jwks *JWKS) func(http.Handler) http.Handler {
       return func(next http.Handler) http.Handler {
           return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
               token := extractBearer(r)
               claims, err := ParseAndVerify(token, jwks)
               if err != nil {
                   httpx.WriteError(w, 401, "unauthorized", "invalid token")
                   return
               }
               ctx := context.WithValue(r.Context(), userKey, &User{ID: claims.Sub})
               next.ServeHTTP(w, r.WithContext(ctx))
           })
       }
   }
   ```

4. **Handlers retrieve the user:**

   ```go
   func (h *Handler) HandleProjects(w http.ResponseWriter, r *http.Request) {
       user, ok := auth.UserFromContext(r.Context())
       if !ok {
           httpx.WriteError(w, 401, "unauthorized", "")
           return
       }
       projects, err := h.store.ListByUser(r.Context(), user.ID)
       // ...
   }
   ```

5. **Mount per-route:**

   ```go
   authMW := auth.RequireAuth(jwks)
   mux.Handle("GET /me", authMW(http.HandlerFunc(meHandler)))
   ```

### Adding a new auth method (e.g., session cookies, OAuth provider, magic links)

Same pattern, different middleware. Each auth method is its own factory in `internal/platform/auth/<method>/`. The middleware always ends with `context.WithValue(r.Context(), userKey, ...)` so handlers don't care HOW the user was authenticated — only that they ARE.

This is the "single output type, multiple input mechanisms" pattern. Critical for sanity as you add OAuth, magic links, API tokens for tools, etc.

### Multi-method endpoints

Some endpoints need to accept either a JWT OR an API key (e.g., webhook endpoints). Compose middleware:

```go
func RequireAuthOrAPIKey(jwks *JWKS, apiKey string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if r.Header.Get("Authorization") != "" {
                // Try JWT
                ...
            } else if r.Header.Get("X-API-Key") != "" {
                // Try API key
                ...
            } else {
                httpx.WriteError(w, 401, "unauthorized", "no credentials")
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

---

## 12. Caching

### Current state: no caching.

This is correct for now. Premature caching is worse than no caching — adds complexity, hides bugs (stale data), and provides zero benefit until you have hot read paths.

### When to add caching

Concrete signals, not feelings:

- A specific endpoint returns the same data 100+ times/min
- A specific DB query takes >50ms and is called repeatedly
- An external API call (LLM, scraper) costs money per request and the result is reusable
- Page-load latency dominated by API calls, measured in production

### Tiers, easiest first

#### A. In-process cache (`golang.org/x/sync/singleflight` + a simple map)

```go
import "github.com/jackc/pgx/v5/pgxpool"
import "golang.org/x/sync/singleflight"

type Store struct {
    pool *pgxpool.Pool
    sf   singleflight.Group  // de-duplicates concurrent fetches
    cache sync.Map            // key → cachedValue
}
```

Use when:
- Data changes rarely
- Cache size is bounded (you can hold the whole working set in memory)
- One process is enough (no horizontal scaling concerns)

This is what you should reach for FIRST. No new dependency, no new infrastructure.

#### B. Redis (when in-process isn't enough)

Add a Redis container next to Postgres. Use `github.com/redis/go-redis/v9`. Wire it into a new shared package `internal/platform/cache/`:

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}
```

The interface (NOT the Redis client directly) is what tools see. Lets you swap implementations later (memcache, in-process LRU, etc.) without changing tool code.

#### C. Postgres advisory locks / unlogged tables (when "we already have Postgres, why add Redis?")

Sometimes the right answer. `UNLOGGED TABLE` writes faster than logged ones; advisory locks coordinate workers. Use when the data already lives in Postgres anyway.

### Cache key discipline

- Always namespace: `reelhooks:profile:{handle}` not `profile:{handle}` — prevents tool collisions
- Encode the version: `reelhooks:v2:profile:{handle}` so a code change can rotate keys without flushing
- Key length matters at scale; keep them short

### Invalidation

The hard part. Two patterns:

1. **TTL-only** — easy, slightly stale. Good default.
2. **TTL + write-through invalidation** — when you write, delete the cached version. Works fine for single-writer scenarios; fragile for multi-writer.

**Never** invalidate from a goroutine while another request might read. Use the cache library's atomic ops.

---

## 13. Logging

### `log/slog` (stdlib)

Set up in `cmd/api/main.go`:

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
slog.SetDefault(logger)
```

Every log line is structured JSON. `docker logs sypher-api | jq` for human reading.

### What to log

- **Every request** — already done by `withLogging` middleware
- **Errors that aren't surfaced to users** — DB failures, external API timeouts
- **Significant state transitions** — startup, shutdown, migration applied, container restarted
- **Operations that cost real money** — outbound LLM calls with their cost in cents

### What NOT to log

- **Secrets** — DSN, API keys, JWT contents (log claim subject, not the token)
- **Raw user PII** — emails, phone numbers, IPs (log salted hashes)
- **Request bodies in full** — log a hash or shape, not the content
- **Stack traces in handlers** — only in `withRecover` for panics; ordinary errors don't need stacks

### Log discipline

```go
// Good — structured, sensitive data hashed
h.logger.Error("waitlist insert failed",
    "err", err,
    "email_hash", security.HashIP(h.salt, req.Email)[:16],
)

// Bad — leaks the email
h.logger.Error("waitlist insert failed", "err", err, "email", req.Email)

// Bad — leaks the entire request
h.logger.Error("got bad request", "err", err, "body", string(body))
```

---

## 14. Errors

### The pattern

Functions return `(value, error)`, not exceptions. Caller decides what to do.

```go
foo, err := someThing()
if err != nil {
    return fmt.Errorf("doing the thing: %w", err)
}
```

The `%w` verb wraps. Callers up the chain can `errors.Is(err, OriginalSentinel)` to detect specific cases.

### Sentinel errors

Define at package level, not inside functions:

```go
var ErrInvalidEmail = errors.New("invalid email")
var ErrNotFound = errors.New("not found")
```

Handlers map sentinels to HTTP status:

```go
switch {
case errors.Is(err, ErrInvalidEmail):
    httpx.WriteError(w, 400, "bad_email", "invalid email")
case errors.Is(err, ErrNotFound):
    httpx.WriteError(w, 404, "not_found", "")
default:
    h.logger.Error("unhandled", "err", err)
    httpx.WriteError(w, 500, "internal_error", "")
}
```

### When to wrap, when not to

```go
// Wrap when adding context
return nil, fmt.Errorf("fetch user %d: %w", id, err)

// Don't wrap when just propagating
return err
```

If wrapping doesn't add information, just return.

### Panics

The only place a panic should occur in production is via `panic()` for a programming error (impossible state). Real errors are returned. Panics from any handler are caught by `withRecover` middleware and logged; the request gets a 500 envelope.

---

## 15. Testing

(Currently zero tests in the repo. This section documents the conventions we'll use as we add them.)

### Test file layout

Tests live next to the code: `handler_test.go` next to `handler.go`, in the same package. Go's convention.

### Table-driven tests

```go
func TestRequest_Validate(t *testing.T) {
    cases := []struct {
        name    string
        email   string
        wantErr error
    }{
        {"valid", "user@example.com", nil},
        {"empty", "", ErrInvalidEmail},
        {"no @", "user.example.com", ErrInvalidEmail},
        {"too long", strings.Repeat("a", 250) + "@x.io", ErrInvalidEmail},
        {"display name format rejected", "Foo <foo@bar.com>", ErrInvalidEmail},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            r := Request{Email: c.email}
            r.Normalize()
            err := r.Validate()
            if !errors.Is(err, c.wantErr) {
                t.Fatalf("got %v, want %v", err, c.wantErr)
            }
        })
    }
}
```

`t.Run` makes each row its own subtest — fail messages tell you which row failed.

### Handler tests via `httptest`

```go
func TestWaitlistHandler_validEmail(t *testing.T) {
    store := &fakeStore{insertOK: true, created: true}
    h := NewHandler(store, "test-salt", slog.Default())

    body := strings.NewReader(`{"email":"a@b.io","source":"test"}`)
    req := httptest.NewRequest("POST", "/waitlist", body)
    rec := httptest.NewRecorder()

    h.ServeHTTP(rec, req)

    if rec.Code != 200 { t.Fatalf("status %d", rec.Code) }
}
```

This requires `Handler` to take an interface (a `signupInserter`) instead of `*Store`. Add the interface at the consumer side, in handler.go.

### Integration tests against a real DB

Use `testcontainers-go` to spin up a real Postgres in CI:

```go
import "github.com/testcontainers/testcontainers-go/modules/postgres"

func TestStore_Insert_integration(t *testing.T) {
    if testing.Short() { t.Skip("integration test") }

    ctx := context.Background()
    container, _ := postgres.RunContainer(ctx, ...)
    defer container.Terminate(ctx)
    // ... apply migrations, create store, run real INSERT
}
```

Run unit tests cheaply (`go test ./...`), integration tests on demand (`go test -run Integration ./...`).

### Coverage discipline

- Every new handler: at least the happy path and one error path
- Every store function: integration test against real Postgres
- Every middleware: a test asserting the wrap behaves as expected
- Validation methods: table-driven, lots of cases

Don't chase 100% — chase "every behavior we care about has a regression test."

---

## 16. The rules — never violate these

These aren't preferences. Violating them causes real bugs.

| # | Rule | Why |
|---|---|---|
| 1 | **No tool package may import another tool package.** | Cross-tool coupling; if shared, lift to `internal/platform/*` or `internal/httpx/etc.` |
| 2 | **Every I/O function takes `context.Context` as its first param.** | Cancellation propagation; client disconnects free up DB connections |
| 3 | **Every `*pgxpool.Pool` is created once at startup, never per-request.** | Connection storms otherwise |
| 4 | **Every `Query` is followed by `defer rows.Close()`.** | Otherwise connections leak |
| 5 | **Every transaction has `defer tx.Rollback(ctx)` after `Begin`.** | Safety — Commit makes the rollback a no-op; failures auto-rollback |
| 6 | **No raw `panic`s in production code.** | Use `(value, error)`. Panics are for impossible programmer errors only. |
| 7 | **Never log raw PII.** | Salted hashes only. Even debug logs leak in production. |
| 8 | **Never `w.Write([]byte("ok"))` directly — use `httpx.WriteJSON`.** | Consistent response shape; debuggable Content-Type |
| 9 | **Every public function/struct/field has a doc comment.** | `golint` enforces it; readers thank you |
| 10 | **Migrations are idempotent and never edited after merge.** | Re-deploys, fresh-VM bootstraps, hand-runs all become safe |
| 11 | **No new dependencies without justification.** | Each `go get` is a security + maintenance commitment |
| 12 | **`gofmt` + `go vet` clean before commit.** | Non-negotiable; CI rejects otherwise |
| 13 | **Secrets only via env vars, never in code or migrations.** | If a value would change between dev and prod, it's an env var |
| 14 | **Tool routes are namespace-prefixed (`/reel-hooks/...`, `/markets/...`).** | Avoids collision; matches the URL design at api.sypher.in |
| 15 | **Tools with ≥5 endpoints expose a single `RegisterRoutes(mux, mw)` function** instead of mounting routes from `server.go` directly. | server.go must stay small as the API grows; surface stays grep-able |
| 16 | **No tool sub-package nesting deeper than one level** (`internal/typography/projects/` is fine; `internal/typography/projects/v2/` is not). | Versioning belongs in the URL or codebase migration, not in folder depth |

---

## 17. Deploy lifecycle

```
local code change
  │
  ▼
git push origin main
  │
  ▼
GitHub Actions
  ├─ vet + build + test (~2 min)
  └─ multi-arch image build + push to ghcr.io (~3-4 min)
  │
  ▼
ghcr.io/thebharathproject/sypher-api:latest
ghcr.io/thebharathproject/sypher-api:sha-<commit>
  │
  ▼
ssh sypher-vm
bash ~/services/sypher-api/deploy.sh
  ├─ docker login ghcr.io       (idempotent)
  ├─ docker pull <image>:latest
  ├─ docker run --rm <image> migrate    (one-shot — applies new migrations)
  ├─ docker stop sypher-api && docker rm sypher-api
  ├─ docker run -d --name sypher-api <image>
  └─ curl http://127.0.0.1:8002/health  (probe — fails if unhealthy)
```

Rollback:

```bash
IMAGE_TAG=sha-<previous-commit> bash ~/services/sypher-api/deploy.sh
```

The deploy script doesn't change between releases — it always pulls `:latest` (or whatever tag you specify) and replaces the container.

---

## 18. Local development

### Setup once

```bash
brew install go
go install golang.org/x/tools/gopls@latest          # language server
brew install golangci-lint                           # linter
go install github.com/cosmtrek/air@latest            # hot-reload (optional)
```

### Daily

```bash
# 1. Open SSH tunnel to the VM Postgres (terminal 1)
ssh sypher-vm                                        # has LocalForward 5432

# 2. Set up env (terminal 2)
cd sypher-api
cp .env.example .env                                 # only if .env doesn't exist
# Edit .env with real DATABASE_URL (using the tunnel) + any test API keys

# 3. Run
export $(grep -v '^#' .env | xargs)
go run ./cmd/api                                     # or `air` for hot-reload

# Smoke test
curl http://127.0.0.1:8000/health
```

### Testing locally against real DB

```bash
go run ./cmd/migrate    # applies migrations against the tunneled DB
go test ./...           # unit tests
go test -run Integration ./...   # integration tests against the real DB
```

### Debugging

`delve` (`brew install delve`) is the Go debugger. VS Code's Go extension uses it under the hood. For ad-hoc:

```bash
dlv debug ./cmd/api
(dlv) break internal/waitlist/handler.go:42
(dlv) continue
```

Or just sprinkle `slog.Info(...)` and `docker logs`. 90% of "I need a debugger" moments are actually just "I need better logs."

---

## 19. Operational cheatsheet

```bash
# Logs (structured JSON; pipe to jq for readability)
docker logs -f sypher-api | jq

# Errors only
docker logs sypher-api 2>&1 | jq 'select(.level == "ERROR")'

# Last hour
docker logs --since 1h sypher-api

# Restart without redeploying
docker restart sypher-api

# What env vars did the container start with?
docker inspect sypher-api --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -v PATH

# Open psql to the database the api writes to
docker exec -it sypher-postgres psql -U sypher -d sypher

# Quick "are signups landing?" query
docker exec sypher-postgres psql -U sypher -d sypher \
  -c "SELECT count(*), date_trunc('day', created_at) FROM waitlist.signups GROUP BY 2 ORDER BY 2 DESC;"

# CPU / memory of the container
docker stats sypher-api --no-stream

# Pin to specific commit (rollback)
IMAGE_TAG=sha-abc1234 bash ~/services/sypher-api/deploy.sh
```

---

## 20. Go idioms reference

If you're new to Go, these are the patterns you'll see throughout the codebase. Each has a one-line "why."

| Idiom | Example | Why |
|---|---|---|
| **`if err != nil { return err }`** | After every fallible call | Errors are values, not exceptions; explicit propagation |
| **`(T, error)` return** | `func Get() (User, error)` | Callers always know success vs failure |
| **`%w` in `fmt.Errorf`** | `fmt.Errorf("doing X: %w", err)` | Lets `errors.Is`/`As` walk the chain |
| **Sentinel errors** | `var ErrNotFound = errors.New("not found")` | Comparable error markers without typed exception hierarchies |
| **Pointer receivers** | `func (h *Handler) Method()` | Mutate state; avoid copies |
| **Value receivers** | `func (r Request) String() string` | Read-only methods on small structs |
| **`defer` for cleanup** | `defer rows.Close()` | LIFO cleanup, runs on return AND on panic |
| **`context.Context` first param** | `func (s *Store) Get(ctx context.Context, ...)` | Cancellation, deadlines, request-scoped values |
| **Constructor `NewX`** | `func NewHandler(...) *Handler` | DI; never expose `&Handler{}` direct construction |
| **`var _ Iface = (*T)(nil)`** | At end of file | Compile-time assertion that `*T` implements `Iface` |
| **Closure as middleware factory** | `func requireAPIKey(key string) func(http.Handler) http.Handler` | Inject config without globals |
| **Struct field tags** | `json:"email" db:"email"` | Wire-format mapping; encoding/json reads these |
| **`accept interfaces, return structs`** | take `Inserter`, return `*Store` | Flexible inputs, concrete outputs; small consumer-side interfaces |
| **`for ... range rows`** | After `pool.Query()` | Don't forget `defer rows.Close()` and check `rows.Err()` |
| **Channels are rare** | Use mutex for shared state, channels for typed pipelines | Most services use zero channels — that's fine |
| **`cmd/<binary>/main.go`** | Binary entry points live in `cmd/` | Convention; multi-binary modules do this |
| **`internal/`** | Most code goes here | Compiler enforces "module-internal" |

---

## When in doubt

- **Look at `internal/waitlist/`** — it's the smallest, most complete tool example
- **Look at Appendix A** for what a Tier-2 tool with async work looks like
- **Run `go vet`** before push; it catches real bugs
- **Read [Effective Go](https://go.dev/doc/effective_go)** every 6 months
- **Ask** in PR review — Go culture is "the simpler, the better"; if you're reaching for cleverness, push back on yourself

---

## Appendix A: Reel Hooks — Tier 2 worked example

Tier 2 in the abstract (§4) is easy to get wrong. This appendix shows what the layout actually looks like for **Reel Hooks** — sypher's Tool 01 — including the new patterns it introduces (async background work, an external-API pipeline). Use this as the reference when scaffolding any new tool of similar complexity.

### What Reel Hooks is

The product spec (from earlier brainstorming):

- A user tracks **2 Instagram creator profiles**
- For each profile, **the top 10 reels** are fetched and analyzed
- **Weekly auto-refresh** (background work)
- Each reel gets a transcript, hook archetype breakdown, and 3 "repurpose for me" concepts
- ₹99/month subscription, gated via Razorpay webhook (matches Pegasus
  billing — see `internal/billing/` and `docs/adr/0006-razorpay-billing.md`)
- ~10 endpoints expected → **Tier 2**

The new pattern Reel Hooks introduces compared to `waitlist`: **async background work**. The Apify → yt-dlp → Deepgram → Claude pipeline takes minutes — handlers can't run it inline. So we add two new sub-packages: `pipeline/` (pure logic, no HTTP) and `worker/` (the loop that drives the pipeline).

### Folder layout

```
internal/reelhooks/
│
├── routes.go               # RegisterRoutes — the canonical HTTP surface,
│                           # mounts every endpoint in one function
├── module.go               # Module struct + New() + Start() + Stop() —
│                           # wires handlers/store/pipeline/worker; manages
│                           # the background goroutine's lifecycle
├── types.go                # shared DTOs across handlers: Profile, Reel,
│                           # Analysis, JobStatus, ReelHookArchetype
├── errors.go               # sentinel errors: ErrProfileNotFound,
│                           # ErrJobInProgress, ErrQuotaExceeded
│
├── handlers/               # ALL HTTP handlers — file per resource
│   ├── profiles.go         # /reel-hooks/profiles*       — tracked-profile CRUD
│   ├── reels.go            # /reel-hooks/.../reels*      — list + detail
│   ├── analyses.go         # /reel-hooks/reels/{id}/...  — hook breakdown + repurpose
│   ├── jobs.go             # /reel-hooks/refresh, /jobs  — trigger + status
│   └── webhooks.go         # /reel-hooks/webhooks/stripe — subscription state
│
├── store/                  # ALL DB access — file per table-group
│   ├── profiles.go         # reel_hooks.tracked_profiles
│   ├── reels.go            # reel_hooks.reels + reel_hooks.transcripts
│   ├── analyses.go         # reel_hooks.hook_analyses + reel_hooks.repurpose_drafts
│   └── jobs.go             # reel_hooks.jobs (async-work tracking)
│
├── pipeline/               # PURE LOGIC — no HTTP, no DB. Inputs → outputs.
│   ├── pipeline.go         # Orchestrator: profile_id → list-of-analyzed-reels
│   ├── apify.go            # Wrap Apify Instagram scraper
│   ├── transcribe.go       # Wrap Deepgram (or yt-dlp + Whisper)
│   ├── analyze.go          # Wrap Claude Haiku — hook archetype + repurpose
│   └── prompts.go          # All prompt templates in one place — reviewable + versionable
│
└── worker/                 # The loop that drives the pipeline
    └── worker.go           # Polls jobs table, calls pipeline, writes results,
                            # updates job status. Started by Module.Start().
```

**14 files for ~10 endpoints.** The discipline pays the moment a fifth resource enters: `audiotracks/`, `hashtags/`, etc. just become more files in `handlers/` + `store/`, never bigger files.

### The HTTP surface

```
GET    /reel-hooks/profiles                       List the user's tracked profiles
POST   /reel-hooks/profiles                       Add a profile to track (rate-limited)
DELETE /reel-hooks/profiles/{id}                  Stop tracking
GET    /reel-hooks/profiles/{id}/reels            List analyzed reels for a profile
GET    /reel-hooks/reels/{id}                     One reel + transcript
GET    /reel-hooks/reels/{id}/analysis            Hook archetype breakdown
POST   /reel-hooks/reels/{id}/repurpose           Generate 3 reel concepts in user's voice
POST   /reel-hooks/refresh                        Trigger refresh for the user's profiles
GET    /reel-hooks/jobs/{id}                      Job status (pending/running/done/failed)
POST   /reel-hooks/webhooks/stripe                Stripe subscription events
```

Ten endpoints. Five handler files. Tier 2 fits exactly.

### Migrations — one per domain entity

```
migrations/
├── 0001_waitlist.sql                              (existing)
├── 0002_reelhooks_profiles.sql                    tracked_profiles
├── 0003_reelhooks_reels.sql                       reels + transcripts (FK to profiles)
├── 0004_reelhooks_analyses.sql                    hook_analyses + repurpose_drafts
├── 0005_reelhooks_jobs.sql                        jobs (FK to profiles)
└── 0006_reelhooks_indexes.sql                     non-trivial indexes — added once shapes settle
```

Each file is idempotent. `0006` is separate because indexes added later are a different concern from the initial table shape; keeping them in their own file makes the change reviewable in isolation.

### Module — the new lifecycle pattern

`Module` is the tool's external face. It owns wiring, owns the background worker's lifecycle, and exposes exactly two public methods to `cmd/api/main.go`:

```go
// internal/reelhooks/module.go
package reelhooks

import (
    "context"
    "log/slog"

    "github.com/jackc/pgx/v5/pgxpool"

    "github.com/TheBharathProject/sypher-api/internal/reelhooks/handlers"
    "github.com/TheBharathProject/sypher-api/internal/reelhooks/pipeline"
    "github.com/TheBharathProject/sypher-api/internal/reelhooks/store"
    "github.com/TheBharathProject/sypher-api/internal/reelhooks/worker"
)

// Config bundles the env-var-driven knobs the tool needs. Lives here so
// the cmd/api/main.go wiring code can pass typed config in one struct,
// rather than 5 individual params.
type Config struct {
    Apify               string // Apify API token
    Deepgram            string // Deepgram API key
    Anthropic           string // Anthropic API key
    StripeWebhookSecret string // for webhook signature verification
}

type Module struct {
    profiles *handlers.Profiles
    reels    *handlers.Reels
    analyses *handlers.Analyses
    jobs     *handlers.Jobs
    webhooks *handlers.Webhooks

    worker *worker.Worker
    logger *slog.Logger
}

func New(pool *pgxpool.Pool, cfg Config, logger *slog.Logger) *Module {
    profileStore  := store.NewProfiles(pool)
    reelStore     := store.NewReels(pool)
    analysisStore := store.NewAnalyses(pool)
    jobStore      := store.NewJobs(pool)

    pipe := pipeline.New(cfg.Apify, cfg.Deepgram, cfg.Anthropic, logger)

    return &Module{
        profiles: handlers.NewProfiles(profileStore, jobStore, logger),
        reels:    handlers.NewReels(reelStore, logger),
        analyses: handlers.NewAnalyses(analysisStore, pipe, logger),
        jobs:     handlers.NewJobs(jobStore, logger),
        webhooks: handlers.NewWebhooks(cfg.StripeWebhookSecret, logger),

        worker: worker.New(jobStore, pipe, logger),
        logger: logger,
    }
}

// Start kicks off the background worker. Called from main() after the
// HTTP server is wired but before it starts listening.
//
// The ctx passed here is the same root ctx the HTTP server uses, so the
// worker exits cleanly when SIGTERM arrives — no separate shutdown
// signalling needed.
func (m *Module) Start(ctx context.Context) {
    go m.worker.Run(ctx)
}

// Stop is currently a no-op. Reserved for future use (flushing in-flight
// jobs, persisting state, etc.). Kept here so main() always calls it —
// future cleanup work has a hook to land in without changing main.
func (m *Module) Stop() {}
```

### Routes — the canonical surface

```go
// internal/reelhooks/routes.go
package reelhooks

import "net/http"

// RegisterRoutes mounts every Reel Hooks endpoint. Caller passes the
// middleware they want applied across the tool — typically requireAuth
// from internal/server. Webhooks are mounted WITHOUT auth because Stripe
// authenticates via the webhook signature, not via Authorization header.
func (m *Module) RegisterRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
    // Profiles
    mux.Handle("GET    /reel-hooks/profiles",                auth(http.HandlerFunc(m.profiles.List)))
    mux.Handle("POST   /reel-hooks/profiles",                auth(http.HandlerFunc(m.profiles.Create)))
    mux.Handle("DELETE /reel-hooks/profiles/{id}",           auth(http.HandlerFunc(m.profiles.Delete)))

    // Reels
    mux.Handle("GET    /reel-hooks/profiles/{id}/reels",     auth(http.HandlerFunc(m.reels.ListByProfile)))
    mux.Handle("GET    /reel-hooks/reels/{id}",              auth(http.HandlerFunc(m.reels.Get)))

    // Analyses
    mux.Handle("GET    /reel-hooks/reels/{id}/analysis",     auth(http.HandlerFunc(m.analyses.Get)))
    mux.Handle("POST   /reel-hooks/reels/{id}/repurpose",    auth(http.HandlerFunc(m.analyses.Repurpose)))

    // Jobs
    mux.Handle("POST   /reel-hooks/refresh",                 auth(http.HandlerFunc(m.jobs.TriggerRefresh)))
    mux.Handle("GET    /reel-hooks/jobs/{id}",               auth(http.HandlerFunc(m.jobs.Status)))

    // Webhooks — Stripe authenticates via signature, no JWT
    mux.Handle("POST   /reel-hooks/webhooks/stripe",         http.HandlerFunc(m.webhooks.Stripe))
}
```

Adding endpoint #11 is a one-line edit, in the file where it belongs. server.go is untouched.

### Wiring in `cmd/api/main.go`

```go
// cmd/api/main.go
func run(logger *slog.Logger) error {
    cfg, err := config.Load()
    if err != nil { return err }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    pool, err := db.NewPool(ctx, cfg.DatabaseURL)
    if err != nil { return err }
    defer pool.Close()

    // Reel Hooks module: handlers + background worker
    rh := reelhooks.New(pool, reelhooks.Config{
        Apify:               cfg.ApifyAPIToken,
        Deepgram:            cfg.DeepgramAPIKey,
        Anthropic:           cfg.AnthropicAPIKey,
        StripeWebhookSecret: cfg.StripeWebhookSecret,
    }, logger)
    rh.Start(ctx)        // background worker starts here
    defer rh.Stop()

    srv := server.New(cfg, pool, logger, rh)  // server gets the module
    return srv.Start(ctx)
}
```

### `server.go::routes()` — three lines for the entire tool

```go
func (s *Server) routes() http.Handler {
    mux := http.NewServeMux()

    mux.HandleFunc("GET /", s.handleRoot)
    mux.Handle("GET /health", health.NewHandler(s.pool))

    // Waitlist (Tier 1 — direct mount)
    apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
    mux.Handle("POST /waitlist",
        apiKey(waitlist.NewHandler(waitlist.NewStore(s.pool), s.cfg.IPSalt, s.logger)))

    // Reel Hooks (Tier 2 — delegate to the module)
    s.reelhooks.RegisterRoutes(mux, requireAuth)

    var h http.Handler = mux
    h = withCORS(h, s.cfg.CORSOrigins)
    h = withLogging(h, s.logger)
    h = withRecover(h, s.logger)
    return h
}
```

server.go stays the same size whether Reel Hooks has 10 or 100 endpoints.

### Where to put what

The "I need to write X — which file does it go in?" lookup table:

| You're writing | Goes in |
|---|---|
| HTTP request parsing, status mapping | `handlers/<resource>.go` |
| SQL query, row mapping | `store/<table-group>.go` |
| External API client (Apify, Deepgram, Claude) | `pipeline/<service>.go` |
| Async-work loop, polling, retries | `worker/worker.go` |
| Prompt templates, model config | `pipeline/prompts.go` |
| Cross-handler types (Profile, Reel) | `types.go` |
| Sentinel errors used by 2+ files | `errors.go` |
| Wiring of all the above | `module.go` |
| Route table | `routes.go` |

### What this example demonstrates beyond the abstract Tier 2

1. **Async background work fits the modular monolith** — `worker.Run(ctx)` is a goroutine started at boot. No Celery, no Redis queue (yet). Postgres + a `jobs` table is enough for the first 1,000 jobs/day.

2. **External-API code goes in `pipeline/`, never in handlers** — keeps handlers thin, makes the pipeline pure-function-testable, lets you mock the API clients without touching HTTP.

3. **Prompt templates are version-controlled and reviewable** — `pipeline/prompts.go` is the place to look when "the LLM is being weird." Single source of truth.

4. **`Module.Start(ctx)` and `Module.Stop()` are reserved hooks** — every tool with background work uses this shape so `cmd/api/main.go` looks the same regardless of which tool. New tool's worker plugs in identically.

5. **Webhooks bypass auth-middleware on purpose** — Stripe authenticates via signature in the body. Mounting at `/reel-hooks/webhooks/stripe` without `auth(...)` is correct, not a bug. Worth a comment in the routes file.

When you build the next tool with similar shape (markets data ingestion, scraping pipelines, anything async), copy this layout and rename. The pattern compounds.
