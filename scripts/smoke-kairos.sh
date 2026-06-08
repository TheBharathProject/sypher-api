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
# The script exits non-zero on the first unexpected response.

set -euo pipefail

API="${API:-http://localhost:8000}"
TOKEN="${TOKEN:?TOKEN env var required — see comments at top}"

# ── helpers ──────────────────────────────────────────────────────────────────

# Pretty-prints a JSON body if jq exists; otherwise raw.
fmt() { if command -v jq >/dev/null 2>&1; then jq . ; else cat ; fi }

# get expects (label, path, [expected_status]); default 200
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
  if [ "$status" != "$want" ]; then
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
  if [ "$status" != "$want" ]; then
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
  if [ "$status" != "$want" ]; then
    echo "!!! $label failed: got $status expected $want" >&2
    exit 1
  fi
}

echo "=== Kairos smoke ============================================="
echo "API:   $API"
echo "Token: ${TOKEN:0:16}..."
echo "=============================================================="
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
NEXT_THU=$(date -v+Thu +%Y-%m-%d 2>/dev/null || date -d "next Thursday" +%Y-%m-%d 2>/dev/null || echo "2026-05-22")
get "Chain (empty OK)" "/kairos/options/chain?underlying=NIFTY&expiry=$NEXT_THU" 200

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

echo
echo "✓ All checks passed."
