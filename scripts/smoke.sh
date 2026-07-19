#!/usr/bin/env bash
# Live smoke test: submit → confirm → read back via /api/preview.
#
# Targets a running server at BASE_URL (default http://localhost:8080), or
# builds and starts one itself. Needs a real OPENAI_API_KEY; with SHEET_ID +
# GOOGLE_APPLICATION_CREDENTIALS configured it appends to the real sheet
# (a clearly marked test lead, safe to delete), otherwise it exercises the
# dry-run destination. A timestamp nonce defeats the same-day dedup, so the
# test is repeatable.
set -u

BASE_URL="${BASE_URL:-http://localhost:8080}"
NONCE="smoke-$(date +%s)"
SERVER_PID=""

pass() { echo "PASS: $*"; exit 0; }
fail() { echo "FAIL: $*"; exit 1; }

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if ! curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "smoke: no server at $BASE_URL — building and starting one"
  BIN="$(mktemp -d)/ziga-smoke"
  go build -o "$BIN" ./cmd/server || fail "server build failed"
  "$BIN" &
  SERVER_PID=$!
  up=""
  for _ in $(seq 1 30); do
    sleep 1
    if curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then up=1; break; fi
  done
  [ -n "$up" ] || fail "server did not become healthy within 30s"
fi

echo "smoke: submitting test lead ($NONCE)"
LEAD="SMOKE TEST $NONCE — Jane Smoke, jane+$NONCE@example.com, needs a smoke-test row appended (source: smoke script). Safe to delete."
resp=$(curl -sS -X POST "$BASE_URL/api/submit" -F "text=$LEAD") || fail "submit request failed"
id=$(printf '%s' "$resp" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
[ -n "$id" ] || fail "no submission id in submit response: $resp"

echo "smoke: confirming submission $id"
cresp=$(curl -sS -X POST "$BASE_URL/api/submissions/$id/confirm" \
  -H 'Content-Type: application/json' -d '{"fields":{}}') || fail "confirm request failed"
printf '%s' "$cresp" | grep -q '"status":"written"' || fail "confirm did not write: $cresp"

echo "smoke: reading back the preview"
presp=$(curl -sS "$BASE_URL/api/preview") || fail "preview request failed"
printf '%s' "$presp" | grep -q "$NONCE" || fail "preview does not show the smoke lead: $presp"

pass "lead $NONCE submitted, confirmed, and visible in the sheet preview"
