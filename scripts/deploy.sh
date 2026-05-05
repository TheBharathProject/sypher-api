#!/usr/bin/env bash
# sypher-api deploy — runs on the OCI VM. No `git clone` involved.
#
# What it does:
#   1. Ensures the sypher-net Docker network exists and sypher-postgres is on it
#   2. (idempotent) docker login ghcr.io using ~/.ghcr-auth
#   3. Pulls the latest image from ghcr.io
#   4. Runs migrations as a one-off container on the same network
#   5. Replaces the running sypher-api container with one from the new image
#   6. Probes /health to confirm it's actually serving
#
# Prerequisites on the VM:
#   - Docker installed
#   - sypher-postgres container running, name "sypher-postgres"
#   - ~/.pg-secret  with DATABASE_URL, WAITLIST_API_KEY, IP_SALT
#   - ~/.ghcr-auth  with GHCR_USERNAME, GHCR_TOKEN  (PAT with read:packages)
#
# Usage:
#   bash deploy.sh                          # latest
#   IMAGE_TAG=sha-abc1234 bash deploy.sh    # pin to a specific commit (rollback)

set -euo pipefail

IMAGE="ghcr.io/thebharathproject/sypher-api"
TAG="${IMAGE_TAG:-latest}"
FULL="$IMAGE:$TAG"
NETWORK="sypher-net"

# ── 1. Load secrets ──────────────────────────────────────────────
for f in "$HOME/.pg-secret" "$HOME/.ghcr-auth"; do
  if [ ! -r "$f" ]; then
    echo "!!! $f missing or unreadable" >&2
    exit 1
  fi
done

set -a
. "$HOME/.pg-secret"
. "$HOME/.ghcr-auth"
set +a

for v in DATABASE_URL WAITLIST_API_KEY IP_SALT GHCR_USERNAME GHCR_TOKEN; do
  if [ -z "${!v:-}" ]; then
    echo "!!! $v not set in secrets" >&2
    exit 1
  fi
done

# DB URL rewritten so containers on the sypher-net network reach Postgres
# via its network alias "postgres". Postgres still binds to 127.0.0.1 on
# the host (for SSH-tunneled local dev) — this gives us both paths.
DB_URL_FOR_CONTAINER="${DATABASE_URL/127.0.0.1/postgres}"

# ── 2. Ensure shared Docker network + Postgres attached to it ────
if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
  echo ">>> creating docker network $NETWORK"
  docker network create "$NETWORK" >/dev/null
fi

# Check if sypher-postgres exists at all (deploy can't proceed without it)
if ! docker ps -a --format '{{.Names}}' | grep -q '^sypher-postgres$'; then
  echo "!!! sypher-postgres container not found — Postgres must be running first" >&2
  exit 1
fi

# Connect Postgres to the network with alias "postgres" (idempotent)
if ! docker network inspect "$NETWORK" \
    --format '{{range .Containers}}{{.Name}} {{end}}' \
    | grep -q sypher-postgres; then
  echo ">>> connecting sypher-postgres to $NETWORK as alias 'postgres'"
  docker network connect --alias postgres "$NETWORK" sypher-postgres
fi

# ── 3. Log in to GHCR (idempotent — credentials cached in ~/.docker/config.json) ──
echo ">>> docker login ghcr.io as $GHCR_USERNAME"
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USERNAME" --password-stdin >/dev/null

# ── 4. Pull image ─────────────────────────────────────────────────
echo ">>> pulling $FULL"
docker pull "$FULL"

# ── 5. Migrations ─────────────────────────────────────────────────
echo ">>> running migrations"
docker run --rm \
  --network "$NETWORK" \
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER" \
  -e WAITLIST_API_KEY="$WAITLIST_API_KEY" \
  -e IP_SALT="$IP_SALT" \
  "$FULL" migrate

# ── 6. Replace the running container ──────────────────────────────
if docker ps -a --format '{{.Names}}' | grep -q '^sypher-api$'; then
  echo ">>> stopping old container"
  docker stop sypher-api >/dev/null
  docker rm sypher-api >/dev/null
fi

echo ">>> starting new container"
docker run -d \
  --name sypher-api \
  --restart unless-stopped \
  --network "$NETWORK" \
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER" \
  -e WAITLIST_API_KEY="$WAITLIST_API_KEY" \
  -e IP_SALT="$IP_SALT" \
  -e CORS_ORIGINS="https://sypher.in,https://www.sypher.in" \
  -e ENV="prod" \
  -p 127.0.0.1:8002:8000 \
  "$FULL" >/dev/null

# ── 7. Wait for healthy ───────────────────────────────────────────
echo ">>> waiting for healthy"
for i in $(seq 1 15); do
  if curl -sf http://127.0.0.1:8002/health >/dev/null; then
    echo ">>> healthy"
    docker ps --filter name=sypher-api
    docker logs --tail 8 sypher-api
    exit 0
  fi
  sleep 1
done

echo "!!! /health did not become healthy in 15s — last logs:" >&2
docker logs --tail 40 sypher-api >&2
exit 1
