#!/usr/bin/env bash
# sypher-api deploy — runs on the OCI VM. No `git clone` involved.
#
# What it does:
#   1. (idempotent) docker login ghcr.io using ~/.ghcr-auth
#   2. Pulls the latest image from ghcr.io
#   3. Runs migrations as a one-off container
#   4. Replaces the running sypher-api container with one from the new image
#   5. Probes /health to confirm it's actually serving
#
# Prerequisites on the VM:
#   - Docker installed
#   - sypher-postgres container running on 127.0.0.1:5432
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

# DB url rewritten so the container can reach the host's Postgres
DB_URL_FOR_CONTAINER="${DATABASE_URL/127.0.0.1/host.docker.internal}"

# ── 2. Log in to GHCR (idempotent — credentials cached in ~/.docker/config.json) ──
echo ">>> docker login ghcr.io as $GHCR_USERNAME"
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USERNAME" --password-stdin >/dev/null

# ── 3. Pull image ─────────────────────────────────────────────────
echo ">>> pulling $FULL"
docker pull "$FULL"

# ── 4. Migrations ─────────────────────────────────────────────────
echo ">>> running migrations"
docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER" \
  -e WAITLIST_API_KEY="$WAITLIST_API_KEY" \
  -e IP_SALT="$IP_SALT" \
  "$FULL" migrate

# ── 5. Replace the running container ──────────────────────────────
if docker ps -a --format '{{.Names}}' | grep -q '^sypher-api$'; then
  echo ">>> stopping old container"
  docker stop sypher-api >/dev/null
  docker rm sypher-api >/dev/null
fi

echo ">>> starting new container"
docker run -d \
  --name sypher-api \
  --restart unless-stopped \
  --add-host=host.docker.internal:host-gateway \
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER" \
  -e WAITLIST_API_KEY="$WAITLIST_API_KEY" \
  -e IP_SALT="$IP_SALT" \
  -e CORS_ORIGINS="https://sypher.in,https://www.sypher.in" \
  -e ENV="prod" \
  -p 127.0.0.1:8002:8000 \
  "$FULL" >/dev/null

# ── 6. Wait for healthy ───────────────────────────────────────────
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
