# sypher-api

Backend API service for [sypher.in](https://sypher.in). Runs as a single Go binary in a Docker container on the OCI VM, behind Caddy at `https://api.sypher.in`. Talks to the local Postgres via `host.docker.internal:5432`.

This is the **modular monolith** that hosts every tool's backend — current tools (`/waitlist`) and future ones (`/reel-hooks/*`, `/markets/*`, `/typography/*`) all live in this one repo, one image, one URL.

> **New here?** Read [`docs/GUIDE.md`](./docs/GUIDE.md) — the contributor's guide. Covers the architecture, the tool-package pattern, how to add a new endpoint or a whole new tool, DB access, auth, caching, the rules. It's the canonical reference for ANY backend change.

## Stack

| | |
|---|---|
| Language | Go 1.25 |
| HTTP router | `net/http` stdlib (1.22+ method+pattern matching) |
| Postgres | `github.com/jackc/pgx/v5` + `pgxpool` |
| Logging | `log/slog` (stdlib, JSON-structured) |
| Config | env vars via `internal/config` |
| Migrations | `cmd/migrate` — walks `migrations/*.sql` in order |
| Image registry | GitHub Container Registry (private; classic PAT) |
| CI | GitHub Actions — vet, build, test on PR; multi-arch GHCR publish on main |

**Total runtime deps: 1 module (pgx + its sub-deps).** No web framework, no ORM, no validator lib. Final image ≈ 25 MB.

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/` | none | service identity |
| GET | `/health` | none | liveness + DB ping |
| POST | `/waitlist` | `X-API-Key` | email signup, idempotent on email |

## Repo layout

```
sypher-api/
├── cmd/
│   ├── api/main.go              # server binary
│   └── migrate/main.go          # one-shot migration runner
├── internal/
│   ├── config/config.go         # env-var parsing, fail-fast
│   ├── db/db.go                 # pgxpool factory + ping
│   ├── server/                  # HTTP plumbing
│   │   ├── server.go            # mux, lifecycle, route registration
│   │   ├── middleware.go        # cors, recover, logging, apiKey factory
│   │   └── respond.go           # writeJSON, writeError
│   ├── health/health.go         # GET /health
│   └── waitlist/                # one folder per tool
│       ├── handler.go           # POST /waitlist
│       ├── store.go             # INSERT ... ON CONFLICT DO NOTHING
│       └── types.go             # request/response shapes
├── migrations/
│   └── 0001_waitlist.sql        # idempotent CREATE for waitlist schema
├── scripts/deploy.sh            # run on VM: pull → migrate → restart → probe
├── .github/workflows/ci.yml     # vet + build + test; multi-arch publish on main
├── Dockerfile                   # multi-stage; final image ~25 MB on alpine
├── entrypoint.sh                # branches: serve → /api  |  migrate → /migrate
├── go.mod / go.sum
└── README.md
```

## Adding a new tool's API

1. Create `internal/<tool>/` with three files: `handler.go`, `store.go`, `types.go`
2. Mount the routes in `internal/server/server.go` — single block per tool, keeps the surface visible
3. If new tables are needed, add `migrations/000N_<tool>.sql` (idempotent SQL)
4. `git push` → CI builds + publishes the new image to GHCR
5. On the VM: `bash ~/services/sypher-api/deploy.sh`

The discipline: **a tool's package may only import `internal/db` and `internal/server`'s public helpers — never another tool's package.** Cross-tool functionality goes into a `internal/platform/` shared package.

## Local development

```bash
# 1. Open the SSH tunnel (one terminal, leave open)
ssh sypher-vm

# 2. In another terminal — env file from template
cd sypher-api
cp .env.example .env
# Edit .env: real DATABASE_URL pointing at the tunnel; WAITLIST_API_KEY can be anything

# 3. Run
export $(grep -v '^#' .env | xargs)
go run ./cmd/api

# Or use air for hot-reload:
#   go install github.com/cosmtrek/air@latest
#   air
```

The server listens on `:8000`. Test:

```bash
curl http://127.0.0.1:8000/
curl http://127.0.0.1:8000/health

curl -sX POST http://127.0.0.1:8000/waitlist \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $WAITLIST_API_KEY" \
  -d '{"email":"local-test@example.com","source":"local-dev"}'
```

## First-time deployment to the VM

The image is **published to GHCR as a private package**. The VM authenticates with a fine-grained Personal Access Token. Setup is one-time.

### Step 1 — Create a classic PAT (browser, ~2 min)

GHCR predates fine-grained PATs and only honors classic tokens for private packages. Create one at https://github.com/settings/tokens/new (classic):

| Field | Value |
|---|---|
| Note | `sypher-vm-ghcr-pull` |
| Expiration | 1 year (set a calendar reminder) |
| Scopes | only `read:packages` |

Copy the `ghp_…` token — you'll only see it once.

### Step 2 — Token + username on the VM

```bash
ssh sypher-vm

cat > ~/.ghcr-auth <<EOF
GHCR_USERNAME=<your-github-username>
GHCR_TOKEN=ghp_<the-token-you-just-copied>
EOF
chmod 600 ~/.ghcr-auth
```

### Step 3 — Drop `deploy.sh` onto the VM (one-time)

```bash
mkdir -p ~/services/sypher-api
curl -fsSL https://raw.githubusercontent.com/TheBharathProject/sypher-api/main/scripts/deploy.sh \
  -o ~/services/sypher-api/deploy.sh
chmod +x ~/services/sypher-api/deploy.sh
```

### Step 4 — Verify `~/.pg-secret`

```bash
cat ~/.pg-secret
# Must contain:
#   POSTGRES_PASSWORD=...
#   DATABASE_URL=postgresql://sypher:...@127.0.0.1:5432/sypher
#   WAITLIST_API_KEY=...      (openssl rand -hex 32)
#   IP_SALT=...               (openssl rand -hex 16)
```

### Step 5 — Run it

```bash
bash ~/services/sypher-api/deploy.sh
```

The script: `docker login ghcr.io` (idempotent) → pull `:latest` → run `migrate` one-shot → replace running container → probe `/health`.

## Subsequent deploys

```bash
ssh sypher-vm
bash ~/services/sypher-api/deploy.sh
```

Pin to a specific commit for rollback:

```bash
IMAGE_TAG=sha-abc1234 bash ~/services/sypher-api/deploy.sh
```

## Operational

```bash
# Logs (structured JSON, pipe to jq for readability)
docker logs -f sypher-api | jq

# Restart without redeploying
docker restart sypher-api

# Inspect env passed to the container
docker inspect sypher-api --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -v PATH

# psql to the DB the api writes to
docker exec -it sypher-postgres psql -U sypher -d sypher
```

### GHCR token rotation (every ~year)

1. New classic PAT at https://github.com/settings/tokens with `read:packages`
2. Update `GHCR_TOKEN` in `~/.ghcr-auth`
3. `docker logout ghcr.io`
4. `bash ~/services/sypher-api/deploy.sh` — login uses the new token
5. Delete the old PAT in the GitHub UI

Set a calendar reminder when you create the token. Expired tokens fail silently with `unauthorized` on next deploy.

## What this service is NOT

- **Not serverless.** Long-running, stateful pgxpool. Runs on the VM by design.
- **Not the place for Vercel-side concerns** (UI, marketing forms). Those live in `sypher-shell` and call this as a backend.
- **Not the only backend.** Tool-internal pipelines (e.g. the legacy `reel-Downloader`) keep their own services until migrated/retired.

## Related docs

- [`sypher-shell/docs/sypher-factory/self-hosted-postgres.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/self-hosted-postgres.md) — Postgres setup
- [`sypher-shell/docs/sypher-factory/vm-deploy-pattern.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/vm-deploy-pattern.md) — image-only deploy pattern
- [`sypher-shell/docs/sypher-factory/architecture.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/architecture.md) — overall system design
