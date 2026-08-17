package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestServer spins up an in-memory Redis and returns a server bound to it
// plus the miniredis handle for assertions/time manipulation.
func newTestServer(t *testing.T, apiToken string) (*server, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	srv := &server{
		rdb:          rdb,
		redisAddr:    mr.Addr(),
		dbClients:    make(map[int]*redis.Client),
		maxBodyBytes: defaultMaxBodyBytes,
		apiToken:     apiToken,
	}
	t.Cleanup(srv.Close)
	return srv, mr
}

// do builds a request, runs it through the server handler, and returns the
// response recorder.
func do(t *testing.T, srv *server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	return rec
}

// decodeJSON unmarshals a response recorder's body, failing the test on
// invalid JSON.
func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON response %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestSetAndGet(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/mykey", "hello world", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got, _ := mr.Get("mykey"); got != "hello world" {
		t.Fatalf("stored value = %q, want %q", got, "hello world")
	}

	rec = do(t, srv, http.MethodGet, "/mykey", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello world" {
		t.Fatalf("get body = %q, want %q", rec.Body.String(), "hello world")
	}
}

func TestGetMissingKey(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestSetWithExpiration(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/temp?expiration=60", "value", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ttl := mr.TTL("temp"); ttl != 60*time.Second {
		t.Fatalf("ttl = %v, want 60s", ttl)
	}

	// Advance past the TTL; the key should be gone.
	mr.FastForward(61 * time.Second)
	if mr.Exists("temp") {
		t.Fatal("key still present after TTL expiry")
	}
}

func TestSetInvalidExpiration(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, exp := range []string{"abc", "-5"} {
		rec := do(t, srv, http.MethodPost, "/k?expiration="+exp, "v", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expiration=%q: got %d, want 400", exp, rec.Code)
		}
	}
}

func TestSetBodyTooLarge(t *testing.T) {
	srv, _ := newTestServer(t, "")
	srv.maxBodyBytes = 8
	rec := do(t, srv, http.MethodPost, "/big", strings.Repeat("x", 100), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestDelete(t *testing.T) {
	srv, mr := newTestServer(t, "")
	_ = mr.Set("gone", "v")

	rec := do(t, srv, http.MethodDelete, "/gone", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d, want 200", rec.Code)
	}
	if mr.Exists("gone") {
		t.Fatal("key still present after delete")
	}
}

func TestDeleteMissingKey(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/absent", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	// No token -> 401.
	if rec := do(t, srv, http.MethodGet, "/k", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", rec.Code)
	}
	// Wrong token -> 401.
	if rec := do(t, srv, http.MethodGet, "/k", "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}
	// Correct token -> reaches handler (404 since key absent).
	if rec := do(t, srv, http.MethodGet, "/k", "", map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusNotFound {
		t.Fatalf("valid token: got %d, want 404", rec.Code)
	}
}

func TestHashSetGetDelete(t *testing.T) {
	srv, mr := newTestServer(t, "")

	// HSET user1 name Elvis
	if rec := do(t, srv, http.MethodPost, "/user1/name", "Elvis", nil); rec.Code != http.StatusOK {
		t.Fatalf("hset name: got %d (%s)", rec.Code, rec.Body.String())
	}
	// HSET user1 last_name Presley
	if rec := do(t, srv, http.MethodPost, "/user1/last_name", "Presley", nil); rec.Code != http.StatusOK {
		t.Fatalf("hset last_name: got %d", rec.Code)
	}
	if got := mr.HGet("user1", "name"); got != "Elvis" {
		t.Fatalf("stored field = %q, want %q", got, "Elvis")
	}

	// HGET user1 name -> Elvis
	rec := do(t, srv, http.MethodGet, "/user1/name", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "Elvis" {
		t.Fatalf("hget name: got %d %q", rec.Code, rec.Body.String())
	}

	// HDEL user1 name
	if rec := do(t, srv, http.MethodDelete, "/user1/name", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("hdel name: got %d", rec.Code)
	}

	// HGET user1 name -> 404 (nil)
	if rec := do(t, srv, http.MethodGet, "/user1/name", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("hget after del: got %d, want 404", rec.Code)
	}
	// Other field still present.
	if rec := do(t, srv, http.MethodGet, "/user1/last_name", "", nil); rec.Code != http.StatusOK || rec.Body.String() != "Presley" {
		t.Fatalf("hget last_name: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestHashGetMissing(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if rec := do(t, srv, http.MethodGet, "/nohash/nofield", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHashDeleteMissing(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if rec := do(t, srv, http.MethodDelete, "/nohash/nofield", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHashAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	if rec := do(t, srv, http.MethodPost, "/h/f", "v", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hset without token: got %d, want 401", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	srv, mr := newTestServer(t, "s3cret") // health is unauthenticated

	rec := do(t, srv, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	// When Redis is down, health reports 503.
	mr.Close()
	rec = do(t, srv, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestGetEnvWithLegacy(t *testing.T) {
	t.Run("uses new key when set", func(t *testing.T) {
		t.Setenv("REDIS_REST_API_TOKEN", "new")
		t.Setenv("API_TOKEN", "old")
		if got := getEnvWithLegacy("REDIS_REST_API_TOKEN", "API_TOKEN", "fallback"); got != "new" {
			t.Fatalf("got %q, want %q", got, "new")
		}
	})

	t.Run("falls back to legacy key", func(t *testing.T) {
		t.Setenv("API_TOKEN", "old")
		if got := getEnvWithLegacy("REDIS_REST_API_TOKEN", "API_TOKEN", "fallback"); got != "old" {
			t.Fatalf("got %q, want %q", got, "old")
		}
	})

	t.Run("uses fallback when neither set", func(t *testing.T) {
		if got := getEnvWithLegacy("REDIS_REST_API_TOKEN", "API_TOKEN", "fallback"); got != "fallback" {
			t.Fatalf("got %q, want %q", got, "fallback")
		}
	})
}

func TestGetEnvInt64WithLegacy(t *testing.T) {
	t.Run("uses new key when set", func(t *testing.T) {
		t.Setenv("REDIS_REST_MAX_BODY_BYTES", "2048")
		t.Setenv("MAX_BODY_BYTES", "4096")
		if got := getEnvInt64WithLegacy("REDIS_REST_MAX_BODY_BYTES", "MAX_BODY_BYTES", 1); got != 2048 {
			t.Fatalf("got %d, want 2048", got)
		}
	})

	t.Run("falls back to legacy key", func(t *testing.T) {
		t.Setenv("MAX_BODY_BYTES", "4096")
		if got := getEnvInt64WithLegacy("REDIS_REST_MAX_BODY_BYTES", "MAX_BODY_BYTES", 1); got != 4096 {
			t.Fatalf("got %d, want 4096", got)
		}
	})

	t.Run("uses fallback when neither set", func(t *testing.T) {
		if got := getEnvInt64WithLegacy("REDIS_REST_MAX_BODY_BYTES", "MAX_BODY_BYTES", 1); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})

	t.Run("invalid new key value uses fallback, ignoring legacy", func(t *testing.T) {
		t.Setenv("REDIS_REST_MAX_BODY_BYTES", "abc")
		t.Setenv("MAX_BODY_BYTES", "4096")
		if got := getEnvInt64WithLegacy("REDIS_REST_MAX_BODY_BYTES", "MAX_BODY_BYTES", 1); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})
}

func TestBinaryRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})

	if rec := do(t, srv, http.MethodPost, "/bin", payload, nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/bin", "", nil)
	if rec.Body.String() != payload {
		t.Fatalf("binary round-trip mismatch")
	}
}

// --- /v1 routes ---

func TestV1SetAndGetJSON(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/string/mykey", "hello world", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: got %d (%s)", rec.Code, rec.Body.String())
	}
	status := decodeJSON[statusResponse](t, rec)
	if status.Status != "ok" {
		t.Fatalf("status = %q, want %q", status.Status, "ok")
	}
	if got, _ := mr.Get("mykey"); got != "hello world" {
		t.Fatalf("stored value = %q, want %q", got, "hello world")
	}

	rec = do(t, srv, http.MethodGet, "/v1/string/mykey", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "hello world" || val.Encoding != "" {
		t.Fatalf("got value=%q encoding=%q, want value=%q encoding=\"\"", val.Value, val.Encoding, "hello world")
	}
}

func TestV1GetMissingKey(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/string/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	errResp := decodeJSON[errorResponse](t, rec)
	if errResp.Error != "Key not found" {
		t.Fatalf("error = %q, want %q", errResp.Error, "Key not found")
	}
}

func TestV1SetWithExpiration(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/string/temp?expiration=60", "value", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ttl := mr.TTL("temp"); ttl != 60*time.Second {
		t.Fatalf("ttl = %v, want 60s", ttl)
	}
}

func TestV1SetInvalidExpiration(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, exp := range []string{"abc", "-5"} {
		rec := do(t, srv, http.MethodPost, "/v1/string/k?expiration="+exp, "v", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expiration=%q: got %d, want 400", exp, rec.Code)
		}
		errResp := decodeJSON[errorResponse](t, rec)
		if errResp.Error == "" {
			t.Fatalf("expiration=%q: expected non-empty error message", exp)
		}
	}
}

func TestV1SetBodyTooLarge(t *testing.T) {
	srv, _ := newTestServer(t, "")
	srv.maxBodyBytes = 8
	rec := do(t, srv, http.MethodPost, "/v1/string/big", strings.Repeat("x", 100), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestV1Delete(t *testing.T) {
	srv, mr := newTestServer(t, "")
	_ = mr.Set("gone", "v")

	rec := do(t, srv, http.MethodDelete, "/v1/string/gone", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d, want 200", rec.Code)
	}
	status := decodeJSON[statusResponse](t, rec)
	if status.Status != "ok" {
		t.Fatalf("status = %q, want %q", status.Status, "ok")
	}
	if mr.Exists("gone") {
		t.Fatal("key still present after delete")
	}
}

func TestV1DeleteMissingKey(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/v1/string/absent", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestV1Auth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	if rec := do(t, srv, http.MethodGet, "/v1/string/k", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusNotFound {
		t.Fatalf("valid token: got %d, want 404", rec.Code)
	}
}

func TestV1HashSetGetDelete(t *testing.T) {
	srv, mr := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/hash/user1/name", "Elvis", nil); rec.Code != http.StatusOK {
		t.Fatalf("hset name: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, http.MethodPost, "/v1/hash/user1/last_name", "Presley", nil); rec.Code != http.StatusOK {
		t.Fatalf("hset last_name: got %d", rec.Code)
	}
	if got := mr.HGet("user1", "name"); got != "Elvis" {
		t.Fatalf("stored field = %q, want %q", got, "Elvis")
	}

	rec := do(t, srv, http.MethodGet, "/v1/hash/user1/name", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("hget name: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "Elvis" {
		t.Fatalf("hget name = %q, want %q", val.Value, "Elvis")
	}

	if rec := do(t, srv, http.MethodDelete, "/v1/hash/user1/name", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("hdel name: got %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/v1/hash/user1/name", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("hget after del: got %d, want 404", rec.Code)
	}
}

func TestV1HashGetMissingField(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/hash/nohash/nofield", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	errResp := decodeJSON[errorResponse](t, rec)
	if errResp.Error != "Field not found" {
		t.Fatalf("error = %q, want %q", errResp.Error, "Field not found")
	}
}

func TestV1HashDeleteMissingField(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/v1/hash/nohash/nofield", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestV1BinaryValueBase64Encoded(t *testing.T) {
	srv, _ := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})

	if rec := do(t, srv, http.MethodPost, "/v1/string/bin", payload, nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/v1/string/bin", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Encoding != "base64" {
		t.Fatalf("encoding = %q, want %q", val.Encoding, "base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(val.Value)
	if err != nil {
		t.Fatalf("invalid base64 value: %v", err)
	}
	if string(decoded) != payload {
		t.Fatalf("decoded value = %q, want %q", decoded, payload)
	}
}

func TestV1RawAcceptOctetStream(t *testing.T) {
	srv, _ := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})

	if rec := do(t, srv, http.MethodPost, "/v1/string/bin", payload, nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/v1/string/bin", "", map[string]string{"Accept": "application/octet-stream"})
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q, want application/octet-stream", ct)
	}
	if rec.Body.String() != payload {
		t.Fatalf("raw round-trip mismatch: got %q, want %q", rec.Body.String(), payload)
	}
}

func TestV1RawAcceptOctetStreamPlainText(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/string/txt", "hello", nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/txt", "", map[string]string{"Accept": "application/octet-stream"})
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), "hello")
	}
}

func TestV1DBSelectionIsolatesKeys(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/string/k", "db1-value", map[string]string{"X-Redis-DB": "1"}); rec.Code != http.StatusOK {
		t.Fatalf("set on db1: got %d", rec.Code)
	}

	// Not visible on the default DB (0).
	if rec := do(t, srv, http.MethodGet, "/v1/string/k", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get on db0: got %d, want 404", rec.Code)
	}

	// Visible on DB 1.
	rec := do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"X-Redis-DB": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("get on db1: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "db1-value" {
		t.Fatalf("value = %q, want %q", val.Value, "db1-value")
	}

	// The lazily created DB-1 client is tracked for cleanup.
	if len(srv.dbClients) != 1 {
		t.Fatalf("dbClients = %d, want 1", len(srv.dbClients))
	}
}

func TestV1InvalidDBHeader(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"X-Redis-DB": "abc"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	errResp := decodeJSON[errorResponse](t, rec)
	if errResp.Error == "" {
		t.Fatalf("expected non-empty error message")
	}
}

func TestV1NotImplementedNamespaces(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, path := range []string{"/v1/keys/mykey"} {
		rec := do(t, srv, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: got %d, want 501", path, rec.Code)
		}
		errResp := decodeJSON[errorResponse](t, rec)
		if errResp.Error == "" {
			t.Fatalf("%s: expected non-empty error message", path)
		}
	}
}

func TestLegacyRoutesStillWorkAlongsideV1(t *testing.T) {
	srv, _ := newTestServer(t, "")

	// A legacy write followed by a v1 read of the same key (both DB 0).
	if rec := do(t, srv, http.MethodPost, "/legacykey", "legacy value", nil); rec.Code != http.StatusOK {
		t.Fatalf("legacy set: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/legacykey", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 get: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "legacy value" {
		t.Fatalf("value = %q, want %q", val.Value, "legacy value")
	}
}

// --- /v1 edge cases ---

func TestV1EmptyValueRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/string/empty", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/empty", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "" || val.Encoding != "" {
		t.Fatalf("got value=%q encoding=%q, want empty string with no encoding", val.Value, val.Encoding)
	}
}

func TestV1MultibyteUTF8Value(t *testing.T) {
	srv, _ := newTestServer(t, "")
	payload := "héllo wörld 🚀 日本語"

	if rec := do(t, srv, http.MethodPost, "/v1/string/utf8", payload, nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/utf8", "", nil)
	val := decodeJSON[valueResponse](t, rec)
	if val.Encoding != "" {
		t.Fatalf("encoding = %q, want no encoding for valid UTF-8", val.Encoding)
	}
	if val.Value != payload {
		t.Fatalf("value = %q, want %q", val.Value, payload)
	}
}

func TestV1KeyWithSpecialCharacters(t *testing.T) {
	srv, _ := newTestServer(t, "")

	// URL-encoded space and slash-adjacent characters in the key segment.
	rec := do(t, srv, http.MethodPost, "/v1/string/my%20key%3A1", "v", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	rec = do(t, srv, http.MethodGet, "/v1/string/my%20key%3A1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "v" {
		t.Fatalf("value = %q, want %q", val.Value, "v")
	}
}

func TestV1SetZeroExpirationMeansNoExpiry(t *testing.T) {
	srv, mr := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/string/k?expiration=0", "v", nil); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	if ttl := mr.TTL("k"); ttl != 0 {
		t.Fatalf("ttl = %v, want no expiry (0)", ttl)
	}
}

func TestV1DBHeaderZeroIsExplicitDefault(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/string/k", "v", map[string]string{"X-Redis-DB": "0"}); rec.Code != http.StatusOK {
		t.Fatalf("set: got %d", rec.Code)
	}
	// Explicit db=0 must use the shared default client, not a cached extra one.
	if len(srv.dbClients) != 0 {
		t.Fatalf("dbClients = %d, want 0 (db=0 should reuse srv.rdb)", len(srv.dbClients))
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/k", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
}

func TestV1NegativeDBHeaderRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"X-Redis-DB": "-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1HashAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	if rec := do(t, srv, http.MethodPost, "/v1/hash/h/f", "v", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hset without token: got %d, want 401", rec.Code)
	}
}

func TestV1NotImplementedRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	rec := do(t, srv, http.MethodGet, "/v1/keys/mykey", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// --- /v1/list routes (issue #6) ---

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON body: %v", err)
	}
	return string(b)
}

// jsonRaw encodes s as a JSON string, for use as a pipelineItem.Body value
// destined for a raw-body endpoint (e.g. string/hash-field SET).
func jsonRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("failed to marshal JSON string: %v", err)
	}
	return b
}

// jsonRawObj encodes v (typically a struct or slice) as a pipelineItem.Body
// value destined for a JSON-body endpoint (e.g. LPUSH, multi-field HSET).
func jsonRawObj(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON body: %v", err)
	}
	return b
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
}

func valuesOf(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	resp := decodeJSON[valuesResponse](t, rec)
	out := make([]string, len(resp.Values))
	for i, v := range resp.Values {
		if v.Encoding != "" {
			t.Fatalf("unexpected encoding %q on values[%d]", v.Encoding, i)
		}
		out[i] = v.Value
	}
	return out
}

func TestV1ListPushAndRange(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/list/mylist/right", jsonBody(t, pushRequest{Values: []string{"a", "b"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rpush: got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 2 {
		t.Fatalf("rpush length = %d, want 2", got.Length)
	}

	rec = do(t, srv, http.MethodPost, "/v1/list/mylist/left", jsonBody(t, pushRequest{Values: []string{"z"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lpush: got %d", rec.Code)
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 3 {
		t.Fatalf("lpush length = %d, want 3", got.Length)
	}

	if got, err := mr.List("mylist"); err != nil || len(got) != 3 || got[0] != "z" {
		t.Fatalf("miniredis list = %v, err = %v, want [z a b]", got, err)
	}

	rec = do(t, srv, http.MethodGet, "/v1/list/mylist", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lrange: got %d", rec.Code)
	}
	got := valuesOf(t, rec)
	want := []string{"z", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("lrange = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lrange = %v, want %v", got, want)
		}
	}
}

func TestV1ListPushEmptyValuesRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/list/l/left", jsonBody(t, pushRequest{Values: nil}), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1ListPushInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/list/l/left", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1ListLen(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "c"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodGet, "/v1/list/l/len", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 3 {
		t.Fatalf("length = %d, want 3", got.Length)
	}
}

func TestV1ListLenMissingKeyIsZero(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/list/nope/len", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 0 {
		t.Fatalf("length = %d, want 0", got.Length)
	}
}

func TestV1ListIndex(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "c"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodGet, "/v1/list/l/index/0", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[valueResponse](t, rec); got.Value != "a" {
		t.Fatalf("index 0 = %q, want %q", got.Value, "a")
	}

	rec = do(t, srv, http.MethodGet, "/v1/list/l/index/-1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[valueResponse](t, rec); got.Value != "c" {
		t.Fatalf("index -1 = %q, want %q", got.Value, "c")
	}
}

func TestV1ListIndexOutOfRange(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := do(t, srv, http.MethodGet, "/v1/list/l/index/5", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestV1ListIndexInvalid(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/list/l/index/notanumber", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1ListPopLeftAndRight(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "c"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodDelete, "/v1/list/l/left", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lpop: got %d", rec.Code)
	}
	if got := valuesOf(t, rec); len(got) != 1 || got[0] != "a" {
		t.Fatalf("lpop = %v, want [a]", got)
	}

	rec = do(t, srv, http.MethodDelete, "/v1/list/l/right", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rpop: got %d", rec.Code)
	}
	if got := valuesOf(t, rec); len(got) != 1 || got[0] != "c" {
		t.Fatalf("rpop = %v, want [c]", got)
	}

	remaining, _ := mr.List("l")
	if len(remaining) != 1 || remaining[0] != "b" {
		t.Fatalf("remaining = %v, want [b]", remaining)
	}
}

func TestV1ListPopWithCount(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "c", "d", "e"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodDelete, "/v1/list/l/left?count=2", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := valuesOf(t, rec)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("lpop count=2 = %v, want [a b]", got)
	}
}

func TestV1ListPopEmptyIs404(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/v1/list/nope/left", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	errResp := decodeJSON[errorResponse](t, rec)
	if errResp.Error != "Key not found" {
		t.Fatalf("error = %q, want %q", errResp.Error, "Key not found")
	}
}

func TestV1ListPopInvalidCount(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, count := range []string{"0", "-1", "abc"} {
		rec := do(t, srv, http.MethodDelete, "/v1/list/l/left?count="+count, "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("count=%q: got %d, want 400", count, rec.Code)
		}
	}
}

func TestV1ListRangeMissingKeyReturnsEmptyArray(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/list/nope", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	resp := decodeJSON[valuesResponse](t, rec)
	if len(resp.Values) != 0 {
		t.Fatalf("values = %v, want empty", resp.Values)
	}
}

func TestV1ListRangeCustomBounds(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "c", "d"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := do(t, srv, http.MethodGet, "/v1/list/l?start=1&stop=2", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := valuesOf(t, rec)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("lrange 1..2 = %v, want [b c]", got)
	}
}

func TestV1ListRangeInvalidBounds(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, q := range []string{"?start=abc", "?stop=abc"} {
		rec := do(t, srv, http.MethodGet, "/v1/list/l"+q, "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

func TestV1ListRem(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "a", "c", "a"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodDelete, "/v1/list/l", jsonBody(t, remRequest{Value: "a"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[removedResponse](t, rec); got.Removed != 3 {
		t.Fatalf("removed = %d, want 3", got.Removed)
	}

	remaining, _ := mr.List("l")
	if len(remaining) != 2 {
		t.Fatalf("remaining = %v, want 2 elements", remaining)
	}
}

func TestV1ListRemWithCount(t *testing.T) {
	srv, mr := newTestServer(t, "")
	if _, err := mr.Push("l", "a", "b", "a", "c", "a"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodDelete, "/v1/list/l", jsonBody(t, remRequest{Value: "a", Count: 2}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[removedResponse](t, rec); got.Removed != 2 {
		t.Fatalf("removed = %d, want 2", got.Removed)
	}
}

func TestV1ListRemMissingKeyRemovesZero(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/v1/list/nope", jsonBody(t, remRequest{Value: "a"}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := decodeJSON[removedResponse](t, rec); got.Removed != 0 {
		t.Fatalf("removed = %d, want 0", got.Removed)
	}
}

func TestV1ListRemInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodDelete, "/v1/list/l", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1ListBinaryValuesBase64EncodedOnRead(t *testing.T) {
	srv, mr := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})
	if _, err := mr.Push("l", payload); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := do(t, srv, http.MethodGet, "/v1/list/l/index/0", "", nil)
	val := decodeJSON[valueResponse](t, rec)
	if val.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", val.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(val.Value)
	if err != nil || string(decoded) != payload {
		t.Fatalf("decoded = %q (err %v), want %q", decoded, err, payload)
	}

	rec = do(t, srv, http.MethodGet, "/v1/list/l", "", nil)
	resp := decodeJSON[valuesResponse](t, rec)
	if len(resp.Values) != 1 || resp.Values[0].Encoding != "base64" {
		t.Fatalf("lrange values = %+v, want one base64-encoded element", resp.Values)
	}
}

func TestV1ListDBSelection(t *testing.T) {
	srv, _ := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/list/l/left", jsonBody(t, pushRequest{Values: []string{"db1"}}), map[string]string{"X-Redis-DB": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("push db1: got %d", rec.Code)
	}

	rec = do(t, srv, http.MethodGet, "/v1/list/l/len", "", nil)
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 0 {
		t.Fatalf("db0 length = %d, want 0", got.Length)
	}

	rec = do(t, srv, http.MethodGet, "/v1/list/l/len", "", map[string]string{"X-Redis-DB": "1"})
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 1 {
		t.Fatalf("db1 length = %d, want 1", got.Length)
	}
}

func TestV1ListAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	for _, req := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/list/l"},
		{http.MethodDelete, "/v1/list/l"},
		{http.MethodGet, "/v1/list/l/len"},
		{http.MethodGet, "/v1/list/l/index/0"},
		{http.MethodPost, "/v1/list/l/left"},
		{http.MethodPost, "/v1/list/l/right"},
		{http.MethodDelete, "/v1/list/l/left"},
		{http.MethodDelete, "/v1/list/l/right"},
	} {
		rec := do(t, srv, req.method, req.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d, want 401", req.method, req.path, rec.Code)
		}
	}
}

// --- /v1/pipeline (issue #7) ---

func TestV1PipelineBasic(t *testing.T) {
	srv, mr := newTestServer(t, "")

	items := []pipelineItem{
		{Method: "POST", Path: "/v1/string/a", Body: jsonRaw(t, "hello")},
		{Method: "GET", Path: "/v1/string/a"},
		{Method: "GET", Path: "/v1/string/missing"},
	}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}

	resp := decodeJSON[pipelineResponse](t, rec)
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(resp.Results))
	}
	if resp.Results[0].Status != http.StatusOK {
		t.Fatalf("item0 status = %d, want 200", resp.Results[0].Status)
	}
	var status statusResponse
	mustUnmarshal(t, resp.Results[0].Body, &status)
	if status.Status != "ok" {
		t.Fatalf("item0 body = %+v, want status=ok", status)
	}

	if resp.Results[1].Status != http.StatusOK {
		t.Fatalf("item1 status = %d, want 200", resp.Results[1].Status)
	}
	var val valueResponse
	mustUnmarshal(t, resp.Results[1].Body, &val)
	if val.Value != "hello" {
		t.Fatalf("item1 value = %q, want %q", val.Value, "hello")
	}

	if resp.Results[2].Status != http.StatusNotFound {
		t.Fatalf("item2 status = %d, want 404 (no abort on a failed item)", resp.Results[2].Status)
	}

	if got, _ := mr.Get("a"); got != "hello" {
		t.Fatalf("stored a = %q, want %q", got, "hello")
	}
}

func TestV1PipelineMixedCommandTypes(t *testing.T) {
	srv, mr := newTestServer(t, "")

	items := []pipelineItem{
		{Method: "POST", Path: "/v1/hash/user1/name", Body: jsonRaw(t, "Elvis")},
		{Method: "POST", Path: "/v1/list/mylist/left", Body: jsonRawObj(t, pushRequest{Values: []string{"a", "b"}})},
		{Method: "POST", Path: "/v1/hashes/user1", Body: jsonRawObj(t, multiSetRequest{Fields: map[string]string{"last_name": "Presley"}})},
	}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}

	resp := decodeJSON[pipelineResponse](t, rec)
	for i, r := range resp.Results {
		if r.Status != http.StatusOK {
			t.Fatalf("item%d status = %d (%s), want 200", i, r.Status, r.Body)
		}
	}

	if got := mr.HGet("user1", "name"); got != "Elvis" {
		t.Fatalf("hash user1.name = %q, want %q", got, "Elvis")
	}
	if got, _ := mr.List("mylist"); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("list mylist = %v, want [b a]", got)
	}
	if got := mr.HGet("user1", "last_name"); got != "Presley" {
		t.Fatalf("hash user1.last_name = %q, want %q", got, "Presley")
	}
}

func TestV1PipelineSequentialOrdering(t *testing.T) {
	srv, _ := newTestServer(t, "")

	items := []pipelineItem{
		{Method: "POST", Path: "/v1/string/k", Body: jsonRaw(t, "first")},
		{Method: "GET", Path: "/v1/string/k"},
		{Method: "POST", Path: "/v1/string/k", Body: jsonRaw(t, "second")},
		{Method: "GET", Path: "/v1/string/k"},
	}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	resp := decodeJSON[pipelineResponse](t, rec)

	var v1, v2 valueResponse
	mustUnmarshal(t, resp.Results[1].Body, &v1)
	mustUnmarshal(t, resp.Results[3].Body, &v2)
	if v1.Value != "first" || v2.Value != "second" {
		t.Fatalf("got %q then %q, want \"first\" then \"second\" (in-order execution)", v1.Value, v2.Value)
	}
}

func TestV1PipelineInvalidMethod(t *testing.T) {
	srv, _ := newTestServer(t, "")
	items := []pipelineItem{{Method: "PATCH", Path: "/v1/string/k"}}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("outer got %d, want 200 (per-item error, not batch failure)", rec.Code)
	}
	resp := decodeJSON[pipelineResponse](t, rec)
	if resp.Results[0].Status != http.StatusBadRequest {
		t.Fatalf("item status = %d, want 400", resp.Results[0].Status)
	}
}

func TestV1PipelinePathOutsideV1Rejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, path := range []string{"/legacykey", "/health", "v1/string/k", "/v1"} {
		items := []pipelineItem{{Method: "GET", Path: path}}
		rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
		resp := decodeJSON[pipelineResponse](t, rec)
		if resp.Results[0].Status != http.StatusBadRequest {
			t.Fatalf("path %q: item status = %d, want 400", path, resp.Results[0].Status)
		}
	}
}

func TestV1PipelineSelfReferenceRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	items := []pipelineItem{{Method: "POST", Path: "/v1/pipeline", Body: jsonRawObj(t, []pipelineItem{{Method: "GET", Path: "/v1/string/k"}})}}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	resp := decodeJSON[pipelineResponse](t, rec)
	if resp.Results[0].Status != http.StatusBadRequest {
		t.Fatalf("item status = %d, want 400 (no nested pipelines)", resp.Results[0].Status)
	}
}

func TestV1PipelineEmptyRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", "[]", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1PipelineInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1PipelineTooManyItemsRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	items := make([]pipelineItem, maxPipelineItems+1)
	for i := range items {
		items[i] = pipelineItem{Method: "GET", Path: "/v1/string/k"}
	}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1PipelineAuthPropagatesToItems(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	// The outer call itself requires the token.
	items := []pipelineItem{{Method: "GET", Path: "/v1/string/k"}}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}

	// With a valid token on the outer call, items run authenticated too
	// (not a separate 401 per item).
	rec = do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), map[string]string{"Authorization": "Bearer s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: got %d", rec.Code)
	}
	resp := decodeJSON[pipelineResponse](t, rec)
	if resp.Results[0].Status != http.StatusNotFound {
		t.Fatalf("item status = %d, want 404 (reached the handler, not 401)", resp.Results[0].Status)
	}
}

func TestV1PipelineDBHeaderPropagatesToItems(t *testing.T) {
	srv, _ := newTestServer(t, "")

	items := []pipelineItem{{Method: "POST", Path: "/v1/string/k", Body: jsonRaw(t, "db1-value")}}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), map[string]string{"X-Redis-DB": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	// Not visible on db0.
	if rec := do(t, srv, http.MethodGet, "/v1/string/k", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("db0 get: got %d, want 404", rec.Code)
	}
	// Visible on db1.
	rec = do(t, srv, http.MethodGet, "/v1/string/k", "", map[string]string{"X-Redis-DB": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("db1 get: got %d", rec.Code)
	}
}

func TestV1PipelineAcceptHeaderNotForwarded(t *testing.T) {
	srv, _ := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})

	if rec := do(t, srv, http.MethodPost, "/v1/string/bin", payload, nil); rec.Code != http.StatusOK {
		t.Fatalf("seed: got %d", rec.Code)
	}

	// Even if the outer request asks for octet-stream, item results stay
	// JSON-enveloped so the overall pipeline response stays valid JSON.
	items := []pipelineItem{{Method: "GET", Path: "/v1/string/bin"}}
	rec := do(t, srv, http.MethodPost, "/v1/pipeline", jsonBody(t, items), map[string]string{"Accept": "application/octet-stream"})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	resp := decodeJSON[pipelineResponse](t, rec)
	var val valueResponse
	mustUnmarshal(t, resp.Results[0].Body, &val)
	if val.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64 (still the JSON envelope, not raw bytes)", val.Encoding)
	}
}

// --- /v1/strings (MGET/MSET) ---

func TestV1MSet(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/strings", jsonBody(t, msetRequest{Values: map[string]string{"a": "1", "b": "2"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[statusResponse](t, rec); got.Status != "ok" {
		t.Fatalf("status = %q, want %q", got.Status, "ok")
	}
	if got, _ := mr.Get("a"); got != "1" {
		t.Fatalf("stored a = %q, want %q", got, "1")
	}
	if got, _ := mr.Get("b"); got != "2" {
		t.Fatalf("stored b = %q, want %q", got, "2")
	}
}

func TestV1MSetEmptyRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/strings", jsonBody(t, msetRequest{Values: nil}), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1MSetInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/strings", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1MGet(t *testing.T) {
	srv, mr := newTestServer(t, "")
	_ = mr.Set("a", "1")
	_ = mr.Set("c", "3")

	rec := do(t, srv, http.MethodGet, "/v1/strings?keys=a,missing,c", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := decodeJSON[nullableValuesResponse](t, rec)
	if len(got.Values) != 3 {
		t.Fatalf("values = %+v, want 3 entries", got.Values)
	}
	if got.Values[0] == nil || got.Values[0].Value != "1" {
		t.Fatalf("values[0] = %v, want 1", got.Values[0])
	}
	if got.Values[1] != nil {
		t.Fatalf("values[1] = %v, want null (missing key)", got.Values[1])
	}
	if got.Values[2] == nil || got.Values[2].Value != "3" {
		t.Fatalf("values[2] = %v, want 3", got.Values[2])
	}
}

func TestV1MGetMissingKeysParam(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/strings", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1StringNamedMsetOrMgetIsNotShadowed(t *testing.T) {
	srv, _ := newTestServer(t, "")

	// A real key named "mset"/"mget" is still reachable via the singular,
	// per-key path — the batch verbs only exist on the separate "/v1/strings"
	// (plural) tree, so there is no shared position to shadow.
	if rec := do(t, srv, http.MethodPost, "/v1/string/mset", "literal key value", nil); rec.Code != http.StatusOK {
		t.Fatalf("set key named mset: got %d", rec.Code)
	}
	rec := do(t, srv, http.MethodGet, "/v1/string/mset", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get key named mset: got %d", rec.Code)
	}
	if val := decodeJSON[valueResponse](t, rec); val.Value != "literal key value" {
		t.Fatalf("value = %q, want %q", val.Value, "literal key value")
	}
}

func TestV1StringsDBSelection(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/strings", jsonBody(t, msetRequest{Values: map[string]string{"a": "db1"}}), map[string]string{"X-Redis-DB": "1"}); rec.Code != http.StatusOK {
		t.Fatalf("mset db1: got %d", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/v1/strings?keys=a", "", nil)
	if got := decodeJSON[nullableValuesResponse](t, rec); got.Values[0] != nil {
		t.Fatalf("db0 mget = %v, want null (key only set on db1)", got.Values[0])
	}

	rec = do(t, srv, http.MethodGet, "/v1/strings?keys=a", "", map[string]string{"X-Redis-DB": "1"})
	got := decodeJSON[nullableValuesResponse](t, rec)
	if got.Values[0] == nil || got.Values[0].Value != "db1" {
		t.Fatalf("db1 mget = %v, want db1", got.Values[0])
	}
}

func TestV1StringsRequireAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	for _, req := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/strings"},
		{http.MethodGet, "/v1/strings?keys=a"},
	} {
		rec := do(t, srv, req.method, req.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d, want 401", req.method, req.path, rec.Code)
		}
	}
}

// --- /v1/hash bulk & field routes (issue #4) ---

func TestV1HGetAll(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis", "last_name", "Presley")

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeJSON[hashFieldsResponse](t, rec)
	if len(got.Fields) != 2 || got.Fields["name"].Value != "Elvis" || got.Fields["last_name"].Value != "Presley" {
		t.Fatalf("fields = %+v, want name=Elvis last_name=Presley", got.Fields)
	}
}

func TestV1HGetAllMissingKeyIsEmpty(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/hashes/nope", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	got := decodeJSON[hashFieldsResponse](t, rec)
	if len(got.Fields) != 0 {
		t.Fatalf("fields = %+v, want empty", got.Fields)
	}
}

func TestV1HGetAllBinaryValueBase64Encoded(t *testing.T) {
	srv, mr := newTestServer(t, "")
	payload := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x10})
	mr.HSet("user1", "bin", payload)

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1", "", nil)
	got := decodeJSON[hashFieldsResponse](t, rec)
	field, ok := got.Fields["bin"]
	if !ok || field.Encoding != "base64" {
		t.Fatalf("fields[bin] = %+v, want base64-encoded", field)
	}
	decoded, err := base64.StdEncoding.DecodeString(field.Value)
	if err != nil || string(decoded) != payload {
		t.Fatalf("decoded = %q (err %v), want %q", decoded, err, payload)
	}
}

func TestV1MultiFieldHSet(t *testing.T) {
	srv, mr := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/hashes/user1", jsonBody(t, multiSetRequest{Fields: map[string]string{"name": "Elvis", "last_name": "Presley"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[addedResponse](t, rec); got.Added != 2 {
		t.Fatalf("added = %d, want 2", got.Added)
	}
	if got := mr.HGet("user1", "name"); got != "Elvis" {
		t.Fatalf("stored name = %q, want %q", got, "Elvis")
	}

	// Setting the same fields again adds zero new fields (values still update).
	rec = do(t, srv, http.MethodPost, "/v1/hashes/user1", jsonBody(t, multiSetRequest{Fields: map[string]string{"name": "Updated"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[addedResponse](t, rec); got.Added != 0 {
		t.Fatalf("added = %d, want 0 (field already existed)", got.Added)
	}
	if got := mr.HGet("user1", "name"); got != "Updated" {
		t.Fatalf("stored name = %q, want %q", got, "Updated")
	}
}

func TestV1MultiFieldHSetEmptyRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/hashes/user1", jsonBody(t, multiSetRequest{Fields: nil}), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1MultiFieldHSetInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/hashes/user1", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1HKeys(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis", "last_name", "Presley")

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/keys", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := decodeJSON[keysResponse](t, rec)
	want := map[string]bool{"name": true, "last_name": true}
	if len(got.Keys) != 2 {
		t.Fatalf("keys = %v, want 2 entries", got.Keys)
	}
	for _, k := range got.Keys {
		if !want[k] {
			t.Fatalf("unexpected key %q in %v", k, got.Keys)
		}
	}
}

func TestV1HKeysMissingKeyIsEmpty(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/hashes/nope/keys", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := decodeJSON[keysResponse](t, rec); len(got.Keys) != 0 {
		t.Fatalf("keys = %v, want empty", got.Keys)
	}
}

func TestV1HVals(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis")

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/values", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := decodeJSON[valuesResponse](t, rec)
	if len(got.Values) != 1 || got.Values[0].Value != "Elvis" {
		t.Fatalf("values = %+v, want [Elvis]", got.Values)
	}
}

func TestV1HMGet(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis", "last_name", "Presley")

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/mget?fields=name,missing,last_name", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := decodeJSON[nullableValuesResponse](t, rec)
	if len(got.Values) != 3 {
		t.Fatalf("values = %+v, want 3 entries", got.Values)
	}
	if got.Values[0] == nil || got.Values[0].Value != "Elvis" {
		t.Fatalf("values[0] = %v, want Elvis", got.Values[0])
	}
	if got.Values[1] != nil {
		t.Fatalf("values[1] = %v, want null (missing field)", got.Values[1])
	}
	if got.Values[2] == nil || got.Values[2].Value != "Presley" {
		t.Fatalf("values[2] = %v, want Presley", got.Values[2])
	}
}

func TestV1HMGetMissingFieldsParam(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/mget", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1HLen(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "a", "1", "b", "2", "c", "3")

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/len", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 3 {
		t.Fatalf("length = %d, want 3", got.Length)
	}
}

func TestV1HLenMissingKeyIsZero(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodGet, "/v1/hashes/nope/len", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 0 {
		t.Fatalf("length = %d, want 0", got.Length)
	}
}

func TestV1HExists(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis")

	rec := do(t, srv, http.MethodGet, "/v1/hash/user1/name/exists", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[existsResponse](t, rec); !got.Exists {
		t.Fatalf("exists = %v, want true", got.Exists)
	}

	rec = do(t, srv, http.MethodGet, "/v1/hash/user1/nope/exists", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (HEXISTS never 404s)", rec.Code)
	}
	if got := decodeJSON[existsResponse](t, rec); got.Exists {
		t.Fatalf("exists = %v, want false", got.Exists)
	}
}

func TestV1HIncrBy(t *testing.T) {
	srv, _ := newTestServer(t, "")

	rec := do(t, srv, http.MethodPost, "/v1/hash/counters/views/incrby", jsonBody(t, incrByRequest{Increment: 5}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[incrByResponse](t, rec); got.Value != 5 {
		t.Fatalf("value = %d, want 5 (field created from 0)", got.Value)
	}

	rec = do(t, srv, http.MethodPost, "/v1/hash/counters/views/incrby", jsonBody(t, incrByRequest{Increment: -2}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := decodeJSON[incrByResponse](t, rec); got.Value != 3 {
		t.Fatalf("value = %d, want 3", got.Value)
	}
}

func TestV1HIncrByInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv, http.MethodPost, "/v1/hash/counters/views/incrby", "not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestV1HIncrByNonNumericFieldErrors(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "name", "Elvis")

	rec := do(t, srv, http.MethodPost, "/v1/hash/user1/name/incrby", jsonBody(t, incrByRequest{Increment: 1}), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
}

// TestV1HashFieldNamedLikeBulkOpIsNotShadowed proves the singular ("/v1/hash",
// single-field access) and plural ("/v1/hashes", bulk access) namespaces are
// fully independent trees: a field literally named e.g. "keys" is reachable
// via ordinary HGET on the singular path, with no ambiguity or precedence
// tricks involved — unlike putting bulk operations as literal siblings of the
// {field} wildcard would risk.
func TestV1HashFieldNamedLikeBulkOpIsNotShadowed(t *testing.T) {
	srv, mr := newTestServer(t, "")
	mr.HSet("user1", "keys", "this is the field's actual value")

	// The singular path always means "the field named 'keys'" — ordinary HGET.
	rec := do(t, srv, http.MethodGet, "/v1/hash/user1/keys", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if val.Value != "this is the field's actual value" {
		t.Fatalf("value = %q, want the field's actual value", val.Value)
	}

	// The plural path always means "list this hash's field names" — HKEYS.
	rec = do(t, srv, http.MethodGet, "/v1/hashes/user1/keys", "", nil)
	got := decodeJSON[keysResponse](t, rec)
	if len(got.Keys) != 1 || got.Keys[0] != "keys" {
		t.Fatalf("keys = %v, want [\"keys\"]", got.Keys)
	}
}

func TestV1HashBulkDBSelection(t *testing.T) {
	srv, _ := newTestServer(t, "")

	if rec := do(t, srv, http.MethodPost, "/v1/hashes/user1", jsonBody(t, multiSetRequest{Fields: map[string]string{"name": "db1"}}), map[string]string{"X-Redis-DB": "1"}); rec.Code != http.StatusOK {
		t.Fatalf("set db1: got %d", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/v1/hashes/user1/len", "", nil)
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 0 {
		t.Fatalf("db0 length = %d, want 0", got.Length)
	}

	rec = do(t, srv, http.MethodGet, "/v1/hashes/user1/len", "", map[string]string{"X-Redis-DB": "1"})
	if got := decodeJSON[lengthResponse](t, rec); got.Length != 1 {
		t.Fatalf("db1 length = %d, want 1", got.Length)
	}
}

func TestV1HashNewEndpointsRequireAuth(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	for _, req := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/hashes/user1"},
		{http.MethodPost, "/v1/hashes/user1"},
		{http.MethodGet, "/v1/hashes/user1/keys"},
		{http.MethodGet, "/v1/hashes/user1/values"},
		{http.MethodGet, "/v1/hashes/user1/mget?fields=a"},
		{http.MethodGet, "/v1/hashes/user1/len"},
		{http.MethodGet, "/v1/hash/user1/name/exists"},
		{http.MethodPost, "/v1/hash/user1/name/incrby"},
	} {
		rec := do(t, srv, req.method, req.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d, want 401", req.method, req.path, rec.Code)
		}
	}
}
