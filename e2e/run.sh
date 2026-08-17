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
req GET /v1/keys/foo
check_status "GET /v1/keys/foo is 501" 501

echo "-- v1 list commands (issue #6)"
req POST /v1/list/mylist/right -d '{"values": ["a", "b"]}'
check_status "RPUSH mylist [a b] is 200" 200
check_json_field "RPUSH mylist length is 2" '.length' "2"
req POST /v1/list/mylist/left -d '{"values": ["z"]}'
check_status "LPUSH mylist [z] is 200" 200
check_json_field "LPUSH mylist length is 3" '.length' "3"
req GET /v1/list/mylist
check_status "LRANGE mylist is 200" 200
check_json_field "LRANGE mylist is [z,a,b]" '[.values[].value] | join(",")' "z,a,b"
req GET /v1/list/mylist/len
check_status "LLEN mylist is 200" 200
check_json_field "LLEN mylist is 3" '.length' "3"
req GET /v1/list/mylist/index/0
check_status "LINDEX mylist 0 is 200" 200
check_json_field "LINDEX mylist 0 is 'z'" '.value' "z"
req GET /v1/list/mylist/index/-1
check_status "LINDEX mylist -1 is 200" 200
check_json_field "LINDEX mylist -1 is 'b'" '.value' "b"
req GET /v1/list/mylist/index/99
check_status "LINDEX mylist 99 (out of range) is 404" 404
req DELETE /v1/list/mylist/left
check_status "LPOP mylist is 200" 200
check_json_field "LPOP mylist popped 'z'" '.values[0].value' "z"
req DELETE "/v1/list/mylist/right?count=2"
check_status "RPOP mylist count=2 is 200" 200
check_json_field "RPOP mylist popped [b,a]" '[.values[].value] | join(",")' "b,a"
req DELETE /v1/list/nolist/left
check_status "LPOP on empty/missing list is 404" 404
req POST /v1/list/remlist/right -d '{"values": ["x", "y", "x", "x"]}'
check_status "RPUSH remlist [x y x x] is 200" 200
req DELETE /v1/list/remlist -d '{"value": "x"}'
check_status "LREM remlist x is 200" 200
check_json_field "LREM remlist removed 3" '.removed' "3"

echo "-- v1 hash bulk & field commands (issue #4)"
req POST /v1/hashes/user2 -d '{"fields": {"name": "Elvis", "last_name": "Presley"}}'
check_status "multi-HSET user2 is 200" 200
check_json_field "multi-HSET user2 added 2" '.added' "2"
req POST /v1/hashes/user2 -d '{"fields": {"name": "Updated"}}'
check_status "multi-HSET user2 (update) is 200" 200
check_json_field "multi-HSET user2 (update) added 0" '.added' "0"
req GET /v1/hashes/user2
check_status "HGETALL user2 is 200" 200
check_json_field "HGETALL user2 name is Updated" '.fields.name.value' "Updated"
check_json_field "HGETALL user2 last_name is Presley" '.fields.last_name.value' "Presley"
req GET /v1/hashes/nohash
check_status "HGETALL missing key is 200 (empty)" 200
check_json_field "HGETALL missing key has no fields" '.fields | length' "0"
req GET /v1/hashes/user2/keys
check_status "HKEYS user2 is 200" 200
check_json_field "HKEYS user2 has 2 keys" '.keys | length' "2"
req GET /v1/hashes/user2/values
check_status "HVALS user2 is 200" 200
check_json_field "HVALS user2 has 2 values" '.values | length' "2"
req "GET" "/v1/hashes/user2/mget?fields=name,missing,last_name"
check_status "HMGET user2 is 200" 200
check_json_field "HMGET user2 [0] is Updated" '.values[0].value' "Updated"
check_json_field "HMGET user2 [1] is null (missing)" '.values[1]' "null"
req GET /v1/hashes/user2/len
check_status "HLEN user2 is 200" 200
check_json_field "HLEN user2 is 2" '.length' "2"
req GET /v1/hash/user2/name/exists
check_status "HEXISTS user2 name is 200" 200
check_json_field "HEXISTS user2 name is true" '.exists' "true"
req GET /v1/hash/user2/nope/exists
check_status "HEXISTS user2 nope is 200" 200
check_json_field "HEXISTS user2 nope is false" '.exists' "false"
req POST /v1/hash/counters/views/incrby -d '{"increment": 5}'
check_status "HINCRBY counters views (+5) is 200" 200
check_json_field "HINCRBY counters views value is 5" '.value' "5"
req POST /v1/hash/counters/views/incrby -d '{"increment": -2}'
check_status "HINCRBY counters views (-2) is 200" 200
check_json_field "HINCRBY counters views value is 3" '.value' "3"

echo "-- v1 string batch commands: MSET/MGET (no shadowing of real keys)"
req POST /v1/strings -d '{"values": {"batch1": "one", "batch2": "two"}}'
check_status "MSET batch1,batch2 is 200" 200
check_json_field "MSET status is ok" '.status' "ok"
req "GET" "/v1/strings?keys=batch1,missing,batch2"
check_status "MGET batch1,missing,batch2 is 200" 200
check_json_field "MGET [0] is one" '.values[0].value' "one"
check_json_field "MGET [1] is null (missing)" '.values[1]' "null"
check_json_field "MGET [2] is two" '.values[2].value' "two"
req POST /v1/string/mset --data-binary "a real key literally named mset"
check_status "POST /v1/string/mset (real key, not batch) is 200" 200
req GET /v1/string/mset
check_status "GET /v1/string/mset (real key, not batch) is 200" 200
check_json_field "key named mset is unaffected by the batch endpoint" '.value' "a real key literally named mset"

echo "-- legacy routes still work alongside /v1"
req POST /legacykey --data-binary "legacy-value"
check_status "POST /legacykey (legacy) is 200" 200
req GET /v1/string/legacykey
check_status "GET /v1/string/legacykey (v1 read of legacy write) is 200" 200
check_json_field "GET /v1/string/legacykey value" '.value' "legacy-value"

echo
echo "==> Results: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
