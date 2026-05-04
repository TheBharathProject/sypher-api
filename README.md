# sypher-api

Backend API service for [sypher.in](https://sypher.in). Hosts the waitlist endpoint and (in time) read APIs for individual sypher tools that need to surface server-stored data on Vercel-hosted pages.

Runs as a Docker container on the OCI VM in Mumbai, behind Caddy at `https://api.sypher.in`. Connects to the Postgres on the same VM via `host.docker.internal:5432`. Bound only to `127.0.0.1:8002` on the host — public traffic enters through Caddy.

## Stack

| | |
|---|---|
| Framework | FastAPI 0.115 |
| Server | uvicorn (production-ready ASGI) |
| DB driver | asyncpg (async, pooled) |
| Validation | pydantic v2 with EmailStr |
| Python | 3.12 (slim-bookworm base image) |

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/` | none | service identity |
| GET | `/health` | none | liveness + DB reachability |
| POST | `/waitlist` | `X-API-Key` | email signup, idempotent on email |

## Repo layout

```
sypher-api/
├── app/
│   ├── main.py              FastAPI() instance, lifespan, CORS, mounts routers
│   ├── config.py            Env vars parsed once at import time
│   ├── db.py                asyncpg pool factory + acquire() context manager
│   └── routers/
│       ├── health.py        GET /health
│       └── waitlist.py      POST /waitlist
├── migrations/
│   └── 0001_waitlist.sql    idempotent CREATE for the waitlist schema/table
├── scripts/
│   ├── deploy.sh            run on VM: git pull → migrate → rebuild → restart
│   └── migrate.sh           apply migrations/*.sql against DATABASE_URL
├── .github/workflows/ci.yml lint + docker build on PR/main
├── Dockerfile               python:3.12-slim, non-root user, healthcheck baked in
├── .env.example
└── requirements.txt
```

## Local development

You can run the API on your Mac against the VM's Postgres via the SSH tunnel.

```bash
# 1. Open the SSH tunnel (one terminal)
ssh sypher-vm

# 2. In another terminal — install deps
cd sypher-api
python3.12 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# 3. Copy env template and fill it in
cp .env.example .env
# Edit .env — DATABASE_URL points at 127.0.0.1:5432 (the tunnel),
# WAITLIST_API_KEY can be anything for local

# 4. Load env + run
export $(grep -v '^#' .env | xargs)
ENV=dev uvicorn app.main:app --reload --port 8002
```

`http://127.0.0.1:8002/docs` will show the OpenAPI explorer (only when `ENV=dev`).

## Deploying to the VM

First deploy:

```bash
ssh sypher-vm
cd ~/services
git clone https://github.com/TheBharathProject/sypher-api.git
cd sypher-api
bash scripts/deploy.sh   # builds, migrates, runs the container
```

Subsequent deploys (after pushing changes):

```bash
ssh sypher-vm
cd ~/services/sypher-api
bash scripts/deploy.sh   # git pull + rebuild + restart in one command
```

The container is named `sypher-api`, listens on `127.0.0.1:8002`, restarts on host reboot. Logs: `docker logs -f sypher-api`.

## Adding a new endpoint

1. Drop a new file in `app/routers/` (one router per resource)
2. Add `app.include_router(my_router.router, tags=["..."])` in `app/main.py`
3. If it needs new tables, add a migration: `migrations/000N_my_thing.sql`
4. Local-test via `uvicorn app.main:app --reload`
5. Push → on the VM, run `bash scripts/deploy.sh`

The deploy script applies migrations before rebuilding, so a migration that the new code depends on lands first.

## What this service is NOT

- **Not a serverless function host.** Long-running, stateful pool, runs on the VM.
- **Not the place for Vercel-side concerns** (auth, server actions, marketing forms' UI). Those live in `sypher-shell` and call this service as a backend.
- **Not the only backend.** Tool-internal pipelines (e.g., `reel-downloader`) keep their own services. This one is for things that need to be reached from the public-facing apex over HTTP.

## Related docs

- [`sypher-shell/docs/sypher-factory/self-hosted-postgres.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/self-hosted-postgres.md) — the Postgres this service writes to
- [`sypher-shell/docs/sypher-factory/architecture.md`](https://github.com/TheBharathProject/sypher-shell/blob/main/docs/sypher-factory/architecture.md) — overall system design
