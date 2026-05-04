#!/usr/bin/env bash
# sypher-api deploy — runs on the OCI VM.
#
# What it does (no `git clone` involved):
#   1. Pulls the latest image from ghcr.io
#   2. Runs migrations as a one-off container
#   3. Stops the old `sypher-api` container, starts a new one from the new image
#   4. Probes /health to confirm it's actually serving
#
# Prerequisites on the VM:
#   - Docker installed (it is)
#   - sypher-postgres container running on 127.0.0.1:5432
#   - ~/.pg-secret has DATABASE_URL, WAITLIST_API_KEY, IP_SALT
#
# Usage:
#   bash deploy.sh                    # latest
#   IMAGE_TAG=sha-abc1234 bash deploy.sh   # pin to a specific commit

set -euo pipefail

IMAGE="ghcr.io/thebharathproject/sypher-api"
TAG="${IMAGE_TAG:-latest}"
FULL="$IMAGE:$TAG"

# 1. Load env from secret file
if [ ! -r "$HOME/.pg-secret" ]; then
  echo "!!! ~/.pg-secret missing — must contain DATABASE_URL, WAITLIST_API_KEY, IP_SALT" >&2
  exit 1
fi
set -a; . "$HOME/.pg-secret"; set +a

# Sanity-check required vars
for v in DATABASE_URL WAITLIST_API_KEY IP_SALT; do
  if [ -z "${!v:-}" ]; then
    echo "!!! $v not set in ~/.pg-secret" >&2
    exit 1
  fi
done

# DB URL is rewritten so the container can reach the host's Postgres
DB_URL_FOR_CONTAINER="${DATABASE_URL/127.0.0.1/host.docker.internal}"

echo ">>> pulling $FULL"
docker pull "$FULL"

echo ">>> running migrations"
docker run --rm \
  --add-host=host.docker.internal:host-gateway \
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER" \
  -e WAITLIST_API_KEY="$WAITLIST_API_KEY" \
  -e IP_SALT="$IP_SALT" \
  "$FULL" migrate

# 3. Replace the running container (zero data loss; state lives in Postgres)
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

# 4. Wait for /health
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
