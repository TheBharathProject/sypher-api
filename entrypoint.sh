#!/bin/sh
# Container entrypoint. Branches based on the first argument:
#   serve     — run the FastAPI server (default)
#   migrate   — apply pending migrations and exit
#   anything else — exec it raw (useful for one-off psql/python commands)

set -e

cmd="${1:-serve}"
shift || true

case "$cmd" in
  serve)
    exec uvicorn app.main:app --host 0.0.0.0 --port 8000 "$@"
    ;;
  migrate)
    exec python -m app.migrate "$@"
    ;;
  *)
    exec "$cmd" "$@"
    ;;
esac
