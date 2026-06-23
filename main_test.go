package main

import (
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
	t.Cleanup(func() { _ = rdb.Close() })

	return &server{rdb: rdb, maxBodyBytes: defaultMaxBodyBytes, apiToken: apiToken}, mr
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
