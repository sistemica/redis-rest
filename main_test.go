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
