#!/usr/bin/env bash
# Apply every .sql file in migrations/ in order against the DATABASE_URL.
# Idempotent — every migration uses CREATE ... IF NOT EXISTS.

set -euo pipefail

if [ -z "${DATABASE_URL:-}" ]; then
  echo "error: DATABASE_URL not set" >&2
  exit 1
fi

cd "$(dirname "$0")/.."

for f in migrations/*.sql; do
  echo ">>> applying $f"
  docker exec -i sypher-postgres psql "$DATABASE_URL" < "$f"
done

echo "all migrations applied"
