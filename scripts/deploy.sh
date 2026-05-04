#!/usr/bin/env bash
# Deploy the API on the OCI VM.
# Usage:  bash scripts/deploy.sh
# Run from inside the cloned sypher-api directory on the VM.
#
# Prerequisites on the VM:
#  - sypher-postgres container running (Postgres 18, see self-hosted-postgres.md)
#  - ~/.pg-secret with DATABASE_URL, WAITLIST_API_KEY, IP_SALT
#  - Docker installed (it is — set up earlier)

set -euo pipefail

cd "$(dirname "$0")/.."

# 1. Pull latest source
echo ">>> git pull"
git pull --ff-only

# 2. Apply any new migrations
echo ">>> running migrations"
set -a; source ~/.pg-secret; set +a
bash scripts/migrate.sh

# 3. Build the new image
echo ">>> docker build"
docker build -t sypher-api:latest .

# 4. Stop the old container if it exists; replace with the new one
if docker ps -a --format '{{.Names}}' | grep -q '^sypher-api$'; then
  echo ">>> stopping old container"
  docker stop sypher-api >/dev/null
  docker rm sypher-api >/dev/null
fi

# Database URL needs to point at host Postgres from inside the container
DB_URL_FOR_CONTAINER="${DATABASE_URL/127.0.0.1/host.docker.internal}"

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
  sypher-api:latest

# 5. Wait for it to start, then probe /health
echo ">>> waiting for healthy"
sleep 3
for i in $(seq 1 10); do
  if curl -sf http://127.0.0.1:8002/health > /dev/null; then
    echo ">>> healthy"
    docker logs --tail 10 sypher-api
    exit 0
  fi
  sleep 1
done

echo "!!! /health did not become healthy in 10s — last logs:" >&2
docker logs --tail 30 sypher-api >&2
exit 1
