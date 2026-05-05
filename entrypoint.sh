#!/bin/sh
# Container entrypoint. Branches based on the first argument:
#   serve     — run the api server (default)
#   migrate   — apply pending migrations and exit
#   anything else — exec it raw (psql, sh, etc.)

set -e

cmd="${1:-serve}"
shift || true

case "$cmd" in
  serve)
    exec /usr/local/bin/api "$@"
    ;;
  migrate)
    exec /usr/local/bin/migrate "$@"
    ;;
  *)
    exec "$cmd" "$@"
    ;;
esac
