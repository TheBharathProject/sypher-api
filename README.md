# sypher-api

Backend API service for [sypher.in](https://sypher.in). Hosts the waitlist endpoint and (in time) read APIs for individual sypher tools that need to surface server-stored data on Vercel-hosted pages.

Runs as a Docker container on the OCI VM in Mumbai, behind Caddy at `https://api.sypher.in`. Connects to the Postgres on the same VM via `host.docker.internal:5432`. Bound only to `127.0.0.1:8002` on the host — public traffic enters through Caddy.

## Deploy flow

```
git push main
  ↓
GitHub Actions builds multi-arch image (amd64 + arm64)
  ↓
Pushes to ghcr.io/thebharathproject/sypher-api:latest
  ↓
On the VM:  bash ~/deploy.sh
  ↓
  pull latest image → run migrate → replace running container → /health probe
```

The VM **never clones the repo**. It only runs `deploy.sh`. Migrations ship inside the image, so a code change and a schema change deploy together atomically.

## Stack

| | |
|---|---|
| Framework | FastAPI 0.115 |
| Server | uvicorn |
| DB driver | asyncpg (async, pooled) |
| Validation | pydantic v2 with EmailStr |
| Python | 3.12 (slim-bookworm base image) |
| Image registry | GitHub Container Registry (`ghcr.io`) |
| CI | GitHub Actions — multi-arch build + publish on push to main |

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/` | none | service identity |
| GET | `/health` | none | liveness + DB reachability |
| POST | `/waitlist` | `X-API-Key` | email signup, idempotent on email |

OpenAPI explorer at `/docs` is **only enabled when `ENV=dev`** — disabled in production.

## Repo layout

```
sypher-api/
├── app/
│   ├── main.py              FastAPI() instance, lifespan, CORS, mounts routers
│   ├── config.py            Env vars parsed once at import time (fail-fast)
│   ├── db.py                asyncpg pool factory + acquire() context manager
│   ├── migrate.py           One-shot migration runner (entry: `migrate`)
│   └── routers/
│       ├── health.py        GET /health
│       └── waitlist.py      POST /waitlist
├── migrations/              SQL migrations, ship inside the image
│   └── 0001_waitlist.sql
├── scripts/
│   └── deploy.sh            Run on VM: pull → migrate → restart → probe
├── .github/workflows/ci.yml lint + import-check on PR; multi-arch publish on main
├── Dockerfile               python:3.12-slim, non-root, HEALTHCHECK
├── entrypoint.sh            Branches on first arg: `serve` (default) or `migrate`
├── .env.example
└── requirements.txt
```

## Local development

You can run the API on your Mac against the VM's Postgres via the SSH tunnel.

```bash
# 1. Open the SSH tunnel (keep this terminal open)
ssh sypher-vm

# 2. In another terminal — install deps in a venv
cd sypher-api
python3.12 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# 3. Copy env template + fill in
cp .env.example .env
# Edit .env: DATABASE_URL points at 127.0.0.1:5432 (the tunnel),
# WAITLIST_API_KEY can be any string for local

# 4. Load env + run with hot-reload
export $(grep -v '^#' .env | xargs)
ENV=dev uvicorn app.main:app --reload --port 8002
```

`http://127.0.0.1:8002/docs` shows the OpenAPI explorer (because `ENV=dev`).

## First-time deployment to the VM

You do this once, ever.

```bash
ssh sypher-vm

# 1. Make the package public (one-time)
#    Go to https://github.com/orgs/TheBharathProject/packages/container/sypher-api/settings
#    → Danger Zone → Change visibility → Public
#    (Otherwise the VM needs a PAT to pull. Public is simplest for OSS-style repos.)

# 2. Drop the deploy script onto the VM and make it executable
mkdir -p ~/services/sypher-api
curl -fsSL https://raw.githubusercontent.com/TheBharathProject/sypher-api/main/scripts/deploy.sh \
  -o ~/services/sypher-api/deploy.sh
chmod +x ~/services/sypher-api/deploy.sh

# 3. Make sure ~/.pg-secret has the three required vars
cat ~/.pg-secret
# Must contain:
#   POSTGRES_PASSWORD=...
#   DATABASE_URL=postgresql://sypher:...@127.0.0.1:5432/sypher
#   WAITLIST_API_KEY=...      (openssl rand -hex 32)
#   IP_SALT=...               (openssl rand -hex 16)

# 4. Run it
bash ~/services/sypher-api/deploy.sh
```

## Subsequent deploys

After every `git push` to main, GitHub Actions publishes a new image. To pick it up:

```bash
ssh sypher-vm
bash ~/services/sypher-api/deploy.sh
```

That's it. The deploy script doesn't change — same script forever. Pin to a specific commit with:

```bash
IMAGE_TAG=sha-abc1234 bash ~/services/sypher-api/deploy.sh
```

(Useful for rollbacks. Find recent SHAs at https://github.com/TheBharathProject/sypher-api/pkgs/container/sypher-api.)

## Adding a new endpoint

1. Drop a new file in `app/routers/<resource>.py` — one router per resource
2. Add `app.include_router(my_router.router, tags=["..."])` in `app/main.py`
3. If it needs new tables, add a migration: `migrations/000N_my_thing.sql` (idempotent SQL)
4. Local-test via `ENV=dev uvicorn app.main:app --reload`
5. `git push main` → wait for the Actions build (~3-4 min) → SSH to VM → `bash ~/services/sypher-api/deploy.sh`

The deploy script applies migrations before starting the new server, so a migration the new code depends on lands first.

## Rollback

```bash
ssh sypher-vm
IMAGE_TAG=sha-<previous-sha> bash ~/services/sypher-api/deploy.sh
```

If the bad version ran a migration that's already applied, you may need to manually undo schema changes via psql — there's no automated migration-down. Mitigation: keep migrations purely additive when possible.

## Operational

```bash
# logs
docker logs -f --tail 50 sypher-api

# restart without redeploying
docker restart sypher-api

# inspect env passed to the container
docker inspect sypher-api --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -v PATH

# one-off psql via the running Postgres container
docker exec -it sypher-postgres psql -U sypher -d sypher
```

## What this service is NOT

- **Not a serverless function host.** Long-running, stateful asyncpg pool. Running on the VM is the correct shape.
- **Not the place for Vercel-side concerns** (UI, server actions for the marketing site). Those live in `sypher-shell` and call this service as a backend.
- **Not the only backend.** Tool-internal pipelines (e.g., `reel-downloader`) keep their own services. This one is for things that need to be reached from the public-facing apex over HTTP.

## Related docs

- [`sypher-shell/docs/sypher-factory/self-hosted-postgres.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/self-hosted-postgres.md) — the Postgres this service writes to
- [`sypher-shell/docs/sypher-factory/architecture.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/architecture.md) — overall system design
