#!/usr/bin/env bash
# Smoke-test every /kairos/* endpoint against a running sypher-api.
#
# Usage:
#   API=http://localhost:8000 TOKEN=<jwt> bash scripts/smoke-kairos.sh
#
# Where to get TOKEN:
#   1. Start the kairos FE: cd ../kairos && npm run dev
#   2. Open http://localhost:3001/kairos/login → sign in with Google
#   3. In DevTools console: localStorage.getItem("kairos_token")
#   4. Copy that string and export as TOKEN
#
# Or for prod testing:
#   API=https://api.sypher.in TOKEN=... bash scripts/smoke-kairos.sh
#
# Admin endpoints: by default the script asserts they answer 403
# admin_required for a non-admin TOKEN. If your TOKEN belongs to a
# platform admin (auth.users.is_admin), run with ADMIN=1 to exercise
# the admin GETs for real (200/503):
#   API=... TOKEN=... ADMIN=1 bash scripts/smoke-kairos.sh
#
# The script is read-only except one strategy create/delete pair, one
# backtest submit, and one watchlist create/delete pair.
# It exits non-zero on the first unexpected response.

set -euo pipefail

API="${API:-http://localhost:8000}"
TOKEN="${TOKEN:?TOKEN env var required — see comments at top}"
ADMIN="${ADMIN:-}"

# ── helpers ──────────────────────────────────────────────────────────────────

# Pretty-prints a JSON body if jq exists; otherwise raw.
fmt() { if command -v jq >/dev/null 2>&1; then jq . ; else cat ; fi }

# ok_status STATUS WANT — WANT may be a single code ("200") or a
# pipe-separated set ("200|503").
ok_status() { case "|$2|" in *"|$1|"*) return 0 ;; *) return 1 ;; esac }

# get expects (label, path, [expected_status]); default 200.
# expected_status accepts alternation, e.g. "200|503".
get() {
  local label="$1"; local path="$2"; local want="${3:-200}"
  echo "── $label"
  echo "   GET $path"
  local body status
  body=$(curl -sS -o /tmp/kairos-body -w "%{http_code}" \
    -H "Authorization: Bearer $TOKEN" \
    "$API$path")
  status="$body"
  echo "   status: $status (want $want)"
  cat /tmp/kairos-body | fmt | head -30 | sed 's/^/   /'
  echo
  if ! ok_status "$status" "$want"; then
    echo "!!! $label failed: got $status expected $want" >&2
    exit 1
  fi
}

# post expects (label, path, body_json, [expected_status]); default 201/200
post() {
  local label="$1"; local path="$2"; local data="$3"; local want="${4:-201}"
  echo "── $label"
  echo "   POST $path"
  local status
  status=$(curl -sS -o /tmp/kairos-body -w "%{http_code}" \
    -X POST \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$data" \
    "$API$path")
  echo "   status: $status (want $want)"
  cat /tmp/kairos-body | fmt | head -40 | sed 's/^/   /'
  echo
  if ! ok_status "$status" "$want"; then
    echo "!!! $label failed: got $status expected $want" >&2
    exit 1
  fi
}

del() {
  local label="$1"; local path="$2"; local want="${3:-204}"
  echo "── $label"
  echo "   DELETE $path"
  local status
  status=$(curl -sS -o /tmp/kairos-body -w "%{http_code}" \
    -X DELETE \
    -H "Authorization: Bearer $TOKEN" \
    "$API$path")
  echo "   status: $status (want $want)"
  echo
  if ! ok_status "$status" "$want"; then
    echo "!!! $label failed: got $status expected $want" >&2
    exit 1
  fi
}

# noauth (method, path) — request WITHOUT a token must 401.
noauth() {
  local method="$1"; local path="$2"
  local status
  status=$(curl -sS -o /dev/null -w "%{http_code}" -X "$method" "$API$path")
  if [ "$status" != "401" ]; then
    echo "!!! unauthenticated $method $path: got $status expected 401" >&2
    exit 1
  fi
  echo "   $method $path → 401 ✓"
}

# tok_req (method, path, want) — request WITH the token, no body;
# used for the admin-without-admin 403 sweep.
tok_req() {
  local method="$1"; local path="$2"; local want="$3"
  local status
  status=$(curl -sS -o /dev/null -w "%{http_code}" -X "$method" \
    -H "Authorization: Bearer $TOKEN" \
    "$API$path")
  if ! ok_status "$status" "$want"; then
    echo "!!! $method $path: got $status expected $want" >&2
    exit 1
  fi
  echo "   $method $path → $status ✓ (want $want)"
}

# Date helpers (BSD date on macOS, GNU date elsewhere).
TODAY=$(date +%Y-%m-%d)
WEEK_AGO=$(date -v-7d +%Y-%m-%d 2>/dev/null || date -d "7 days ago" +%Y-%m-%d 2>/dev/null || echo "$TODAY")
NEXT_THU=$(date -v+Thu +%Y-%m-%d 2>/dev/null || date -d "next Thursday" +%Y-%m-%d 2>/dev/null || echo "2026-05-22")
ZERO_ID="00000000-0000-0000-0000-000000000000"

echo "=== Kairos smoke ============================================="
echo "API:   $API"
echo "Token: ${TOKEN:0:16}..."
echo "Admin: ${ADMIN:-no (admin endpoints asserted 403)}"
echo "=============================================================="
echo

# ── 0. Auth gate — every endpoint must 401 without a token ───────────────────
echo "── Unauthenticated sweep (every endpoint must 401)"
noauth GET    "/kairos/options/underlyings"
noauth GET    "/kairos/options/expiries"
noauth GET    "/kairos/options/chain"
noauth GET    "/kairos/options/chain/timestamps"
noauth GET    "/kairos/options/analytics/intraday"
noauth POST   "/kairos/strategies"
noauth GET    "/kairos/strategies"
noauth DELETE "/kairos/strategies/$ZERO_ID"
noauth POST   "/kairos/backtest"
noauth GET    "/kairos/backtest/$ZERO_ID"
noauth GET    "/kairos/backtests"
noauth GET    "/kairos/watchlists"
noauth POST   "/kairos/watchlists"
noauth DELETE "/kairos/watchlists/$ZERO_ID"
noauth POST   "/kairos/watchlists/$ZERO_ID/items"
noauth DELETE "/kairos/watchlists/$ZERO_ID/items/$ZERO_ID"
noauth GET    "/kairos/alerts"
noauth POST   "/kairos/alerts"
noauth PATCH  "/kairos/alerts/$ZERO_ID"
noauth DELETE "/kairos/alerts/$ZERO_ID"
noauth GET    "/kairos/screener"
noauth GET    "/kairos/fundamentals/RELIANCE"
noauth GET    "/kairos/announcements"
noauth GET    "/kairos/paper/account"
noauth POST   "/kairos/paper/reset"
noauth POST   "/kairos/paper/orders"
noauth GET    "/kairos/paper/orders"
noauth DELETE "/kairos/paper/orders/$ZERO_ID"
noauth GET    "/kairos/paper/positions"
noauth POST   "/kairos/paper/positions/$ZERO_ID/close"
noauth POST   "/kairos/orders"
noauth GET    "/kairos/provider/status"
noauth GET    "/kairos/marketdata/quotes"
noauth GET    "/kairos/marketdata/candles"
noauth GET    "/kairos/marketdata/indices"
noauth GET    "/kairos/marketdata/search"
noauth GET    "/kairos/marketdata/movers"
noauth GET    "/kairos/marketdata/sectors"
noauth POST   "/kairos/admin/snapshot"
noauth GET    "/kairos/admin/health"
noauth GET    "/kairos/admin/coverage"
noauth GET    "/kairos/admin/coverage/day"
noauth GET    "/kairos/admin/export/chains"
noauth GET    "/kairos/admin/ingest-runs"
noauth GET    "/kairos/admin/partitions"
noauth GET    "/kairos/admin/backtests"
noauth POST   "/kairos/admin/backtests/$ZERO_ID/retry"
noauth GET    "/kairos/admin/users"
noauth PATCH  "/kairos/admin/users/$ZERO_ID"
noauth POST   "/kairos/admin/fundamentals/upload"
echo

# ── 1. Provider status ───────────────────────────────────────────────────────
# Always returns 200; .ready tells us whether Kite is authed.
get "Provider status" "/kairos/provider/status" 200

# ── 2. Underlyings ───────────────────────────────────────────────────────────
get "Underlyings" "/kairos/options/underlyings" 200

# ── 3. Expiries — needs provider; falls through to 503 if not ready ──────────
# Comment out this block while Kite is not configured.
# get "Expiries (NIFTY)" "/kairos/options/expiries?underlying=NIFTY" 200

# ── 4. Chain — works without ingest; returns empty rows ──────────────────────
# Use a near-future Thursday so the date parses. If no ingest has run,
# you'll see rows:[] and staleness_seconds:0.
get "Chain (empty OK)" "/kairos/options/chain?underlying=NIFTY&expiry=$NEXT_THU" 200

# Time-machine scrubber + intraday analytics — empty arrays without ingest.
get "Chain timestamps" "/kairos/options/chain/timestamps?underlying=NIFTY&date=$TODAY" 200
get "Intraday analytics" "/kairos/options/analytics/intraday?underlying=NIFTY&expiry=$NEXT_THU&date=$TODAY" 200

# ── 5. Strategies — full CRUD round-trip ─────────────────────────────────────
post "Save strategy" "/kairos/strategies" '{
  "name": "Smoke straddle",
  "underlying": "NIFTY",
  "legs": [
    {"side":"SELL","optType":"CE","strikeRule":"ATM","expiryRule":"WEEKLY","lots":1},
    {"side":"SELL","optType":"PE","strikeRule":"ATM","expiryRule":"WEEKLY","lots":1}
  ],
  "entryTime": "09:20",
  "exitTime":  "15:15",
  "stopLoss":  30,
  "target":    50
}' 201

# Pull the id we just created. Uses jq if present.
if command -v jq >/dev/null 2>&1; then
  STRATEGY_ID=$(jq -r .id /tmp/kairos-body)
  echo "   captured strategy id: $STRATEGY_ID"
  echo
fi

get "List strategies" "/kairos/strategies" 200

if [ -n "${STRATEGY_ID:-}" ]; then
  del "Delete strategy" "/kairos/strategies/$STRATEGY_ID" 204
fi

# ── 6. Backtest — submit + poll ──────────────────────────────────────────────
# Without real chain data in kairos.option_chains this will return
# status=failed with error="no data available...". That's expected.
post "Submit backtest" "/kairos/backtest" '{
  "name": "Smoke",
  "underlying": "NIFTY",
  "legs": [
    {"side":"SELL","optType":"CE","strikeRule":"ATM","expiryRule":"WEEKLY","lots":1},
    {"side":"SELL","optType":"PE","strikeRule":"ATM","expiryRule":"WEEKLY","lots":1}
  ],
  "fromDate":  "2026-04-01",
  "toDate":    "2026-05-15",
  "entryTime": "09:20",
  "exitTime":  "15:15",
  "stopLoss":  0,
  "target":    0
}' 202

if command -v jq >/dev/null 2>&1; then
  BACKTEST_ID=$(jq -r .id /tmp/kairos-body)
  echo "   captured backtest id: $BACKTEST_ID"
  echo

  echo "── Poll backtest (up to 30s)"
  for i in $(seq 1 15); do
    sleep 2
    curl -sS -H "Authorization: Bearer $TOKEN" \
      "$API/kairos/backtest/$BACKTEST_ID" -o /tmp/kairos-body
    STATUS=$(jq -r .status /tmp/kairos-body 2>/dev/null || echo "?")
    echo "   tick $i: status=$STATUS"
    if [ "$STATUS" = "done" ] || [ "$STATUS" = "failed" ]; then
      cat /tmp/kairos-body | fmt | head -30 | sed 's/^/   /'
      break
    fi
  done
  echo
fi

get "List backtests" "/kairos/backtests" 200

# ── 7. Watchlists — list + one create/delete pair ────────────────────────────
# GET auto-creates the default list on first call.
get "List watchlists" "/kairos/watchlists" 200

post "Create watchlist" "/kairos/watchlists" '{"name":"Smoke list"}' 201
if command -v jq >/dev/null 2>&1; then
  WATCHLIST_ID=$(jq -r .id /tmp/kairos-body)
  echo "   captured watchlist id: $WATCHLIST_ID"
  echo
fi
if [ -n "${WATCHLIST_ID:-}" ]; then
  del "Delete watchlist" "/kairos/watchlists/$WATCHLIST_ID" 204
fi

# ── 8. Alerts — read-only ────────────────────────────────────────────────────
get "List alerts" "/kairos/alerts" 200

# ── 9. Screener + fundamentals + announcements ───────────────────────────────
# Screener serves rows (possibly empty) even with no fundamentals
# ingested; pricesLive=false when the provider can't quote.
get "Screener" "/kairos/screener" 200
# 404 is fine until fundamentals have been uploaded via admin CSV.
get "Fundamental (RELIANCE)" "/kairos/fundamentals/RELIANCE" "200|404"
get "Announcements" "/kairos/announcements" 200

# ── 10. Paper trading — read-only (no orders placed) ─────────────────────────
get "Paper account" "/kairos/paper/account" 200
get "Paper orders" "/kairos/paper/orders" 200
get "Paper positions" "/kairos/paper/positions" 200

# Dark-launched live endpoint: must refuse — 403 while
# KAIROS_LIVE_TRADING is off, 501 once it's on. Never 200.
post "Live orders (must refuse)" "/kairos/orders" '{}' "403|501"

# ── 11. Market data — best-effort, provider-dependent ────────────────────────
# 503 = provider down/unauthed (e.g. Kite token expired), 429 = provider
# rate limit, 502 = upstream hiccup. All acceptable in a smoke run.
MD_OK="200|429|502|503"
get "MD quotes" "/kairos/marketdata/quotes?symbols=RELIANCE,TCS" "$MD_OK"
get "MD candles" "/kairos/marketdata/candles?symbol=RELIANCE&interval=1d&from=$WEEK_AGO&to=$TODAY" "$MD_OK"
get "MD indices" "/kairos/marketdata/indices" "$MD_OK"
get "MD search" "/kairos/marketdata/search?q=REL" "$MD_OK"
get "MD movers" "/kairos/marketdata/movers" "$MD_OK"
get "MD sectors" "/kairos/marketdata/sectors" "$MD_OK"

# ── 12. Admin — 403 for non-admin tokens; real reads with ADMIN=1 ────────────
if [ -z "$ADMIN" ]; then
  echo "── Admin sweep (non-admin token must 403 everywhere)"
  tok_req POST  "/kairos/admin/snapshot" 403
  tok_req GET   "/kairos/admin/health" 403
  tok_req GET   "/kairos/admin/coverage" 403
  tok_req GET   "/kairos/admin/coverage/day" 403
  tok_req GET   "/kairos/admin/export/chains" 403
  tok_req GET   "/kairos/admin/ingest-runs" 403
  tok_req GET   "/kairos/admin/partitions" 403
  tok_req GET   "/kairos/admin/backtests" 403
  tok_req POST  "/kairos/admin/backtests/$ZERO_ID/retry" 403
  tok_req GET   "/kairos/admin/users" 403
  tok_req PATCH "/kairos/admin/users/$ZERO_ID" 403
  tok_req POST  "/kairos/admin/fundamentals/upload" 403
  echo
else
  # Read-only admin endpoints only — snapshot / retry / user-patch /
  # fundamentals-upload mutate, so the smoke run leaves them alone.
  get "Admin health" "/kairos/admin/health" "200|503"
  get "Admin coverage" "/kairos/admin/coverage?underlying=NIFTY&from=$WEEK_AGO&to=$TODAY" "200|503"
  get "Admin coverage day" "/kairos/admin/coverage/day?underlying=NIFTY&date=$TODAY" "200|503"
  get "Admin export chains" "/kairos/admin/export/chains?underlying=NIFTY&from=$TODAY&to=$TODAY" "200|503"
  get "Admin ingest runs" "/kairos/admin/ingest-runs?limit=5" "200|503"
  get "Admin partitions" "/kairos/admin/partitions" "200|503"
  get "Admin backtests" "/kairos/admin/backtests" "200|503"
  get "Admin users" "/kairos/admin/users" "200|503"
fi

echo
echo "✓ All checks passed."
