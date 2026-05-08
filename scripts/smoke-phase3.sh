#!/usr/bin/env bash
# Smoke test for Phase 3 (premium gating + community + notifications).
# Hits each endpoint in sequence; prints PASS/FAIL per check.
#
# Usage:
#   API=http://localhost:8000 TOKEN=eyJ... bash scripts/smoke-phase3.sh
#
# TOKEN: paste your Pegasus JWT (DevTools → Application → Local Storage
#        → sypher_jwt). Required.
# API:   defaults to http://localhost:8000. Set to https://api.sypher.in
#        for prod.
#
# Exits non-zero on any failure so this can run in CI later. Each check
# is read-only or creates one disposable post that's soft-deleted at
# the end.

set -uo pipefail

API="${API:-http://localhost:8000}"
TOKEN="${TOKEN:-}"

if [ -z "$TOKEN" ]; then
  echo "TOKEN env var required (Pegasus JWT)" >&2
  exit 2
fi

# ── helpers ──────────────────────────────────────────────────────────
pass=0
fail=0

check() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "  PASS  $label"
    pass=$((pass + 1))
  else
    echo "  FAIL  $label"
    fail=$((fail + 1))
  fi
}

req() {
  # Args: METHOD PATH [JSON_BODY]
  local method="$1"; local path="$2"; local body="${3:-}"
  local args=(-s -o /tmp/smoke-resp -w "%{http_code}" -X "$method"
              "$API$path" -H "Authorization: Bearer $TOKEN")
  if [ -n "$body" ]; then
    args+=(-H "Content-Type: application/json" -d "$body")
  fi
  curl "${args[@]}"
}

# Wraps `req` so we can use it inside `check`. Exits 0 iff the HTTP
# status is in [expected_min, expected_max].
expect() {
  local min="$1" max="$2"; shift 2
  local code
  code=$(req "$@")
  [ "$code" -ge "$min" ] && [ "$code" -le "$max" ]
}

# ── checks ──────────────────────────────────────────────────────────
echo "Phase 3 smoke against $API"
echo

echo "Health"
check "GET /health → 200" expect 200 200 GET /health

echo
echo "/me — premium fields present"
check "GET /me → 200"               expect 200 200 GET /job-tracker/me
check "/me has isPremium field"     bash -c "jq -e '.isPremium != null' /tmp/smoke-resp"
check "/me has emailNotificationsEnabled" bash -c "jq -e '.emailNotificationsEnabled != null' /tmp/smoke-resp"

echo
echo "Notifications"
check "GET /notifications/count → 200" expect 200 200 GET /job-tracker/notifications/count
check "count payload has unread"       bash -c "jq -e '.unread != null' /tmp/smoke-resp"
check "GET /notifications → 200"       expect 200 200 GET /job-tracker/notifications

echo
echo "Community list (auth)"
for surface in reviews experiences referrals ask recruiters; do
  check "GET /community/$surface → 200" expect 200 200 GET "/job-tracker/community/$surface?limit=5"
done

echo
echo "Community list (public, no auth)"
for surface in reviews experiences referrals ask recruiters; do
  http_code=$(curl -s -o /dev/null -w "%{http_code}" "$API/job-tracker/public/community/$surface?limit=5")
  if [ "$http_code" -eq 200 ]; then
    echo "  PASS  GET /public/community/$surface → 200"
    pass=$((pass + 1))
  else
    echo "  FAIL  GET /public/community/$surface → $http_code"
    fail=$((fail + 1))
  fi
done

echo
echo "Community CRUD round-trip (Ask surface)"
post_id=""
post_body='{"title":"smoke test post — please ignore","body":"created by scripts/smoke-phase3.sh","metadata":{"tags":["smoke"]},"isPublic":false}'
http_code=$(req POST "/job-tracker/community/ask" "$post_body")
if [ "$http_code" -eq 201 ]; then
  post_id=$(jq -r '.id' < /tmp/smoke-resp)
  echo "  PASS  POST /community/ask → 201 (id=$post_id)"
  pass=$((pass + 1))
else
  echo "  FAIL  POST /community/ask → $http_code"
  fail=$((fail + 1))
fi

if [ -n "$post_id" ]; then
  check "GET /community/posts/{id} → 200"  expect 200 200 GET "/job-tracker/community/posts/$post_id"
  check "POST upvote → 204"                expect 204 204 POST "/job-tracker/community/posts/$post_id/vote" '{"value":1}'
  check "GET post returns voteCount=1"     bash -c "curl -s '$API/job-tracker/community/posts/$post_id' -H 'Authorization: Bearer $TOKEN' | jq -e '.voteCount == 1'"

  comment_body='{"body":"smoke comment — please ignore"}'
  http_code=$(req POST "/job-tracker/community/posts/$post_id/comments" "$comment_body")
  if [ "$http_code" -eq 201 ]; then
    comment_id=$(jq -r '.id' < /tmp/smoke-resp)
    echo "  PASS  POST /community/posts/{id}/comments → 201"
    pass=$((pass + 1))
    check "DELETE /community/comments/{id} → 204" expect 204 204 DELETE "/job-tracker/community/comments/$comment_id"
  else
    echo "  FAIL  POST comment → $http_code"
    fail=$((fail + 1))
  fi

  # Cleanup: soft-delete the test post.
  check "DELETE /community/posts/{id} → 204" expect 204 204 DELETE "/job-tracker/community/posts/$post_id"
fi

echo
echo "──────────────────────────────"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
