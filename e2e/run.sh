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

# reqH METHOD PATH EXTRA_HEADER [curl-args...]  -> sets STATUS and BODY
reqH() {
  local method="$1" path="$2" header="$3"; shift 3
  STATUS="$(curl -s -m 10 -o "$tmpbody" -w '%{http_code}' \
    -X "$method" -H "Authorization: Bearer ${TOKEN}" -H "$header" "$@" "${BASE}${path}")"
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

check_json_field() { # desc jq-filter expected
  local got
  got="$(echo "$BODY" | jq -r "$2" 2>/dev/null)"
  if [ "$got" = "$3" ]; then
    echo "  ok  : $1"
    pass=$((pass + 1))
  else
    echo "  FAIL: $1 (expected $2 = '$3', got '$got'; body: $BODY)"
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

echo "-- v1 string set/get/delete (JSON responses)"
req POST /v1/string/foo --data-binary "bar"
check_status "POST /v1/string/foo is 200" 200
check_json_field "POST /v1/string/foo status is ok" '.status' "ok"
req GET /v1/string/foo
check_status "GET /v1/string/foo is 200" 200
check_json_field "GET /v1/string/foo value is 'bar'" '.value' "bar"
req GET /v1/string/missing
check_status "GET /v1/string/missing is 404" 404
check_json_field "GET /v1/string/missing error message" '.error' "Key not found"
req DELETE /v1/string/foo
check_status "DELETE /v1/string/foo is 200" 200
req GET /v1/string/foo
check_status "GET /v1/string/foo after delete is 404" 404

echo "-- v1 content negotiation (raw octet-stream)"
req POST /v1/string/raw --data-binary "raw-bytes"
check_status "POST /v1/string/raw is 200" 200
reqH GET /v1/string/raw "Accept: application/octet-stream"
check_status "GET /v1/string/raw (octet-stream) is 200" 200
check_body   "GET /v1/string/raw (octet-stream) returns raw bytes" "raw-bytes"

echo "-- v1 hash commands"
req POST /v1/hash/user1/name --data-binary "Elvis"
check_status "v1 HSET user1 name is 200" 200
req GET /v1/hash/user1/name
check_status "v1 HGET user1 name is 200" 200
check_json_field "v1 HGET user1 name value" '.value' "Elvis"
req DELETE /v1/hash/user1/name
check_status "v1 HDEL user1 name is 200" 200
req GET /v1/hash/user1/name
check_status "v1 HGET user1 name after delete is 404" 404

echo "-- v1 database selection (X-Redis-DB header)"
reqH POST /v1/string/dbkey "X-Redis-DB: 1" --data-binary "db1-value"
check_status "POST /v1/string/dbkey db=1 is 200" 200
req GET /v1/string/dbkey
check_status "GET /v1/string/dbkey db=0 (default) is 404" 404
reqH GET /v1/string/dbkey "X-Redis-DB: 1"
check_status "GET /v1/string/dbkey db=1 is 200" 200
check_json_field "GET /v1/string/dbkey db=1 value" '.value' "db1-value"
reqH GET /v1/string/dbkey "X-Redis-DB: notanumber"
check_status "GET /v1/string/dbkey with invalid X-Redis-DB is 400" 400

echo "-- v1 reserved namespaces (not yet implemented)"
req GET /v1/list/foo
check_status "GET /v1/list/foo is 501" 501
req GET /v1/keys/foo
check_status "GET /v1/keys/foo is 501" 501

echo "-- legacy routes still work alongside /v1"
req POST /legacykey --data-binary "legacy-value"
check_status "POST /legacykey (legacy) is 200" 200
req GET /v1/string/legacykey
check_status "GET /v1/string/legacykey (v1 read of legacy write) is 200" 200
check_json_field "GET /v1/string/legacykey value" '.value' "legacy-value"

echo
echo "==> Results: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
