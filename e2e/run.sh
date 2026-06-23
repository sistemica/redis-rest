#!/usr/bin/env bash
#
# Full end-to-end test: brings up a Redis-compatible datastore (Valkey by
# default) and the API container via docker compose, runs every API scenario
# against the live stack, then tears everything down.
#
# Usage:
#   ./run.sh                                 # uses Valkey
#   REDIS_IMAGE=redis:7-alpine ./run.sh      # test against Redis instead
#   HOST_PORT=9090 ./run.sh                  # publish API on a different port
#
set -euo pipefail

cd "$(dirname "$0")"

TOKEN="${API_TOKEN:-testtoken}"
HOST_PORT="${HOST_PORT:-8081}"
BASE="http://localhost:${HOST_PORT}"
MAX_BODY_BYTES="${MAX_BODY_BYTES:-65536}"
export API_TOKEN="$TOKEN" HOST_PORT MAX_BODY_BYTES

# docker compose (v2) with a fallback to the legacy docker-compose binary.
if docker compose version >/dev/null 2>&1; then
  DC="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  DC="docker-compose"
else
  echo "ERROR: docker compose is required" >&2
  exit 1
fi

cleanup() { $DC down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> Building and starting stack (datastore: ${REDIS_IMAGE:-valkey/valkey:8-alpine})"
$DC up -d --build

echo "==> Waiting for API to become healthy"
for i in $(seq 1 30); do
  if curl -fsS -m 2 "${BASE}/health" >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: API did not become healthy in time" >&2
    $DC logs >&2 || true
    exit 1
  fi
  sleep 1
done

pass=0
fail=0
tmpbody="$(mktemp)"
trap 'rm -f "$tmpbody"; cleanup' EXIT

# req METHOD PATH [curl-args...]  -> sets STATUS and BODY
req() {
  local method="$1" path="$2"; shift 2
  STATUS="$(curl -s -m 10 -o "$tmpbody" -w '%{http_code}' \
    -X "$method" -H "Authorization: Bearer ${TOKEN}" "$@" "${BASE}${path}")"
  BODY="$(cat "$tmpbody")"
}

check_status() { # desc expected
  if [ "$STATUS" = "$2" ]; then
    echo "  ok  : $1"
    pass=$((pass + 1))
  else
    echo "  FAIL: $1 (expected HTTP $2, got $STATUS; body: $BODY)"
    fail=$((fail + 1))
  fi
}

check_body() { # desc expected
  if [ "$BODY" = "$2" ]; then
    echo "  ok  : $1"
    pass=$((pass + 1))
  else
    echo "  FAIL: $1 (expected body '$2', got '$BODY')"
    fail=$((fail + 1))
  fi
}

echo "==> Running scenarios"

echo "-- health & auth"
STATUS="$(curl -s -m 5 -o "$tmpbody" -w '%{http_code}' "${BASE}/health")"; BODY="$(cat "$tmpbody")"
check_status "GET /health is 200" 200
STATUS="$(curl -s -m 5 -o "$tmpbody" -w '%{http_code}' "${BASE}/foo")"; BODY="$(cat "$tmpbody")"
check_status "GET /foo without token is 401" 401

echo "-- string set/get/delete"
req POST /foo --data-binary "bar"
check_status "POST /foo is 200" 200
req GET /foo
check_status "GET /foo is 200" 200
check_body   "GET /foo returns 'bar'" "bar"
req GET /missing
check_status "GET /missing is 404" 404
req DELETE /foo
check_status "DELETE /foo is 200" 200
req GET /foo
check_status "GET /foo after delete is 404" 404
req DELETE /foo
check_status "DELETE /foo again is 404" 404

echo "-- expiration"
req POST "/tmp?expiration=1" --data-binary "ephemeral"
check_status "POST /tmp?expiration=1 is 200" 200
req GET /tmp
check_status "GET /tmp before expiry is 200" 200
sleep 2
req GET /tmp
check_status "GET /tmp after expiry is 404" 404
req POST "/bad?expiration=-1" --data-binary "x"
check_status "POST /bad?expiration=-1 is 400" 400

echo "-- body size limit (${MAX_BODY_BYTES} bytes)"
big="$(head -c $((MAX_BODY_BYTES + 1024)) /dev/zero | tr '\0' 'x')"
req POST /big --data-binary "$big"
check_status "POST oversized body is 413" 413

echo "-- hash commands (issue #1)"
req POST /user1/name --data-binary "Elvis"
check_status "HSET user1 name is 200" 200
req POST /user1/last_name --data-binary "Presley"
check_status "HSET user1 last_name is 200" 200
req GET /user1/name
check_status "HGET user1 name is 200" 200
check_body   "HGET user1 name returns 'Elvis'" "Elvis"
req DELETE /user1/name
check_status "HDEL user1 name is 200" 200
req GET /user1/name
check_status "HGET user1 name after delete is 404" 404
req GET /user1/last_name
check_body   "HGET user1 last_name still 'Presley'" "Presley"
req GET /user1/nope
check_status "HGET missing field is 404" 404

echo
echo "==> Results: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
