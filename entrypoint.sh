#!/bin/sh
# Container entrypoint. Branches based on the first argument:
#   serve       — run the api server (default)
#   migrate     — apply pending migrations and exit
#   cron-once   — fire a single cron job once and exit
#                 e.g. `docker run --rm <image> cron-once daily-digest`
#                 useful for prod smoke-testing without waiting for the
#                 09:00/03:00 IST tick. Same env vars as serve.
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
  cron-once)
    exec /usr/local/bin/cron-once "$@"
    ;;
  *)
    exec "$cmd" "$@"
    ;;
esac
