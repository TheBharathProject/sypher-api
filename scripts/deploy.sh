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
#   - ~/.pg-secret  with:
#       Required (api refuses to start without these):
#         DATABASE_URL
#         WAITLIST_API_KEY
#         IP_SALT
#         JWT_SECRET                       — long random; rotating evicts everyone
#         GOOGLE_OAUTH_CLIENT_ID           — from Google Cloud Console
#         GOOGLE_OAUTH_CLIENT_SECRET       — from Google Cloud Console
#         GOOGLE_OAUTH_REDIRECT_URL        — https://api.sypher.in/auth/google/callback
#         FRONTEND_LOGIN_REDIRECT_URL      — https://sypher.in/pegasus/auth/callback
#       Optional but recommended (features quietly fail without them):
#         R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET, R2_PUBLIC_URL
#         DEEPSEEK_API_KEY                 — without this, AI features 503
#         RESEND_API_KEY                   — without this, the mailer falls back to slog
#                                            and Phase 2 still ships in-app notifications;
#                                            set it once your sending domain is DKIM verified
#         MAIL_FROM_ADDRESS                — e.g. "hello@sypher.in". Required alongside
#                                            RESEND_API_KEY; without it the mailer noops.
#         MAIL_FROM_NAME                   — e.g. "Pegasus". Defaults to "Pegasus" if unset.
#         RAZORPAY_KEY_ID                  — without this + _SECRET, billing handlers
#                                            return 503 service_unavailable; the rest of
#                                            the app boots fine. Use rzp_test_* for test mode,
#                                            rzp_live_* once KYC is approved.
#         RAZORPAY_KEY_SECRET              — pair to KEY_ID, server-only.
#         RAZORPAY_WEBHOOK_SECRET          — set per-endpoint in Razorpay dashboard;
#                                            HMAC-SHA256 verifies webhook payloads.
#         RAZORPAY_PLAN_ID                 — plan_xxx for ₹99/mo standard recurring tier.
#         RAZORPAY_PLAN_ID_PLUS            — plan_xxx for ₹299/mo Premium+ tier
#                                            (premium + 200 credits/cycle). Optional;
#                                            empty hides the Premium+ card on /upgrade.
#         KAIROS_DATA_PROVIDER             — "kite" (default), "dhan",
#                                            "upstox", "angel", or "null".
#                                            Picks the active data feed for
#                                            Kairos options ingestion. ADR-0011.
#         KAIROS_KITE_API_KEY              — Kite Connect API key (₹500/mo).
#                                            Without it, kairos endpoints
#                                            return 503 and the ingest cron
#                                            logs "provider not configured".
#         KAIROS_KITE_API_SECRET           — pair to API_KEY, server-only.
#         KAIROS_RETENTION_WEEKS           — how many weekly partitions of
#                                            option_chains to keep. Default
#                                            156 (~3 years). ADR-0009 D5.
#         KAIROS_FRONTEND_LOGIN_REDIRECT_URL  per-tool OAuth redirect for
#                                            kairos. Defaults to the global
#                                            FRONTEND_LOGIN_REDIRECT_URL when
#                                            empty. Set to
#                                            "https://sypher.in/kairos/auth/callback"
#                                            in prod.
#         LATEX_SERVICE_URL                — base URL of the LaTeX compile
#                                            sidecar. Without this, Resume
#                                            Builder PDF exports return 503
#                                            and the FE falls back to .tex.
#                                            The sidecar is a hand-rolled
#                                            image (NOT a Docker Hub pull) —
#                                            source lives at ~/sypher-tex on
#                                            the developer machine. The
#                                            HTTP contract mirrors YtoTech's
#                                            latex-on-http but the image
#                                            itself is built from
#                                            ~/sypher-tex/Dockerfile +
#                                            server.py. See
#                                            docs/sypher-tex.md for the
#                                            full runbook. One-time on VM:
#                                              # scp ~/sypher-tex/{Dockerfile,server.py} to VM first
#                                              cd ~/sypher-tex
#                                              docker build -t sypher-tex:latest .
#                                              docker run -d --name sypher-tex \
#                                                --network sypher-net \
#                                                --restart unless-stopped \
#                                                sypher-tex:latest
#                                            Then set LATEX_SERVICE_URL=http://sypher-tex:8080
#                                            (port 8080 — that's what
#                                            server.py binds to; a bare
#                                            http://sypher-tex tries port
#                                            80 and gets connection
#                                            refused).
#       Optional with sane defaults (override only if you know why):
#         JWT_ISSUER (default sypher.in), JWT_AUDIENCE (default sypher.in), JWT_TTL (default 168h)
#         CORS_ORIGINS (default includes https://sypher.in, www.sypher.in, http://localhost:3000)
#         PUBLIC_PROFILE_BASE_URL (defaults to derive-from-FRONTEND_LOGIN_REDIRECT_URL;
#                                  set to "https://sypher.in/u/" in prod so the apex /u/
#                                  doesn't drift under a tool's basePath as more tools land)
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

# Required for the server to even boot — config.go calls required() on these.
REQUIRED_VARS=(
  DATABASE_URL
  WAITLIST_API_KEY
  IP_SALT
  JWT_SECRET
  GOOGLE_OAUTH_CLIENT_ID
  GOOGLE_OAUTH_CLIENT_SECRET
  GOOGLE_OAUTH_REDIRECT_URL
  FRONTEND_LOGIN_REDIRECT_URL
  GHCR_USERNAME
  GHCR_TOKEN
)
for v in "${REQUIRED_VARS[@]}"; do
  if [ -z "${!v:-}" ]; then
    echo "!!! $v not set in secrets" >&2
    exit 1
  fi
done

# Optional — features quietly degrade if missing. We forward whatever's set;
# unset values become empty strings inside the container, which os.Getenv
# returns as "" and the relevant feature checks for that.
OPTIONAL_VARS=(
  JWT_ISSUER
  JWT_AUDIENCE
  JWT_TTL
  CORS_ORIGINS
  AI_USAGE_MONTHLY_TOKEN_LIMIT
  PUBLIC_PROFILE_BASE_URL
  R2_ACCOUNT_ID
  R2_ACCESS_KEY_ID
  R2_SECRET_ACCESS_KEY
  R2_BUCKET
  R2_PUBLIC_URL
  DEEPSEEK_API_KEY
  DEEPSEEK_BASE_URL
  DEEPSEEK_MODEL
  RESEND_API_KEY
  MAIL_FROM_ADDRESS
  MAIL_FROM_NAME
  # Razorpay billing (Phase 6b). Without RAZORPAY_KEY_ID / _SECRET the
  # billing handlers respond 503 and the rest of the app keeps booting.
  # Without RAZORPAY_PLAN_ID_PLUS the Premium+ tier endpoint 503s and
  # the frontend hides that card. See docs/adr/0006-razorpay-billing.md.
  RAZORPAY_KEY_ID
  RAZORPAY_KEY_SECRET
  RAZORPAY_WEBHOOK_SECRET
  RAZORPAY_PLAN_ID
  RAZORPAY_PLAN_ID_PLUS
  SLACK_FEEDBACK_WEBHOOK_URL
  # LaTeX sidecar URL (yotech/latex-on-http). Without it, Resume Builder
  # PDF endpoints return 503. Container is started manually on the VM —
  # see the comment block at the top of this script.
  LATEX_SERVICE_URL
  # Kairos — options research tool. ADR-0011 picks the active data
  # provider; without keys, the cron logs "provider not configured" and
  # /options endpoints fall back to last-stored / 503. The rest of
  # sypher-api boots regardless.
  KAIROS_DATA_PROVIDER
  KAIROS_KITE_API_KEY
  KAIROS_KITE_API_SECRET
  KAIROS_RETENTION_WEEKS
  KAIROS_FRONTEND_LOGIN_REDIRECT_URL
  # Stubs for future providers. Setting these does nothing yet (the
  # impls return ErrNotImplemented); forwarded so the swap is one
  # env-var change when the stubs are filled in.
  KAIROS_DHAN_ACCESS_TOKEN
  KAIROS_UPSTOX_CLIENT_ID
  KAIROS_UPSTOX_CLIENT_SECRET
  KAIROS_ANGEL_API_KEY
  KAIROS_ANGEL_CLIENT_CODE
  KAIROS_ANGEL_PASSWORD
  KAIROS_ANGEL_TOTP_SECRET
)

# Warn (don't fail) on missing optionals so a half-configured deploy is loud.
for v in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET R2_PUBLIC_URL \
         DEEPSEEK_API_KEY RESEND_API_KEY MAIL_FROM_ADDRESS \
         RAZORPAY_KEY_ID RAZORPAY_KEY_SECRET RAZORPAY_WEBHOOK_SECRET \
         RAZORPAY_PLAN_ID RAZORPAY_PLAN_ID_PLUS \
         LATEX_SERVICE_URL \
         KAIROS_KITE_API_KEY KAIROS_KITE_API_SECRET; do
  if [ -z "${!v:-}" ]; then
    echo "??? $v unset — feature(s) depending on it will be degraded" >&2
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

# Build the -e arg list:
#   - DATABASE_URL is rewritten so the container reaches Postgres via the
#     `postgres` network alias instead of the host loopback.
#   - ENV is hardcoded to prod here (the deploy script only ever runs on
#     the prod VM; dev runs come from `go run` against .env, not this).
#   - CORS_ORIGINS falls back to the prod default if not set in secrets.
ENV_FLAGS=(
  -e DATABASE_URL="$DB_URL_FOR_CONTAINER"
  -e ENV="prod"
)
# Forward every required var (already validated non-empty above) by name —
# `-e VAR` without a value tells Docker to inherit from the host process,
# which we just sourced from ~/.pg-secret.
for v in WAITLIST_API_KEY IP_SALT JWT_SECRET \
         GOOGLE_OAUTH_CLIENT_ID GOOGLE_OAUTH_CLIENT_SECRET \
         GOOGLE_OAUTH_REDIRECT_URL FRONTEND_LOGIN_REDIRECT_URL; do
  ENV_FLAGS+=( -e "$v" )
done
# Forward optionals only if set, so a totally-empty optional doesn't override
# a default baked into config.go (e.g. CORS_ORIGINS, JWT_ISSUER).
for v in "${OPTIONAL_VARS[@]}"; do
  if [ -n "${!v:-}" ]; then
    ENV_FLAGS+=( -e "$v" )
  fi
done

echo ">>> starting new container"
docker run -d \
  --name sypher-api \
  --restart unless-stopped \
  --network "$NETWORK" \
  "${ENV_FLAGS[@]}" \
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
