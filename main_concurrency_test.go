package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestConcurrentClientForDB exercises the lazy per-DB client cache under
// concurrent access (run with `go test -race`) and verifies it actually
// dedupes: goroutines racing to first-create a client for a given DB must all
// observe the same *redis.Client instance.
func TestConcurrentClientForDB(t *testing.T) {
	srv, _ := newTestServer(t, "")

	const goroutines = 50
	for db := 1; db <= 5; db++ {
		clients := make(chan *redis.Client, goroutines)
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				clients <- srv.clientForDB(db)
			}()
		}
		wg.Wait()
		close(clients)

		var first *redis.Client
		for c := range clients {
			if first == nil {
				first = c
			} else if c != first {
				t.Fatalf("db %d: goroutines observed distinct client instances, cache did not dedupe", db)
			}
		}
	}

	if len(srv.dbClients) != 5 {
		t.Fatalf("dbClients = %d, want 5", len(srv.dbClients))
	}
}

// TestConcurrentSetGetSameKey hammers a single key from many goroutines to
// surface data races in the request path (run with `go test -race`).
func TestConcurrentSetGetSameKey(t *testing.T) {
	srv, _ := newTestServer(t, "")

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			rec := do(t, srv, http.MethodPost, "/v1/string/hot", fmt.Sprintf("value-%d", i), nil)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent set %d: got %d", i, rec.Code)
			}
		}()
	}
	wg.Wait()

	rec := do(t, srv, http.MethodGet, "/v1/string/hot", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	val := decodeJSON[valueResponse](t, rec)
	if !strings.HasPrefix(val.Value, "value-") {
		t.Fatalf("unexpected final value %q", val.Value)
	}
}

// TestConcurrentDistinctKeysAndDBs spreads concurrent writes across distinct
// keys and DB headers, then verifies every write landed in the right place —
// catching any cross-request state bleed from shared server fields.
func TestConcurrentDistinctKeysAndDBs(t *testing.T) {
	srv, _ := newTestServer(t, "")

	const goroutines = 40
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("/v1/string/k%d", i)
			db := strconv.Itoa(i % 4)
			rec := do(t, srv, http.MethodPost, key, fmt.Sprintf("v%d", i), map[string]string{"X-Redis-DB": db})
			if rec.Code != http.StatusOK {
				t.Errorf("set k%d db=%s: got %d", i, db, rec.Code)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		key := fmt.Sprintf("/v1/string/k%d", i)
		db := strconv.Itoa(i % 4)
		rec := do(t, srv, http.MethodGet, key, "", map[string]string{"X-Redis-DB": db})
		if rec.Code != http.StatusOK {
			t.Fatalf("get k%d db=%s: got %d", i, db, rec.Code)
		}
		val := decodeJSON[valueResponse](t, rec)
		want := fmt.Sprintf("v%d", i)
		if val.Value != want {
			t.Fatalf("k%d db=%s: value = %q, want %q", i, db, val.Value, want)
		}
	}
}

// newBenchServer is the benchmark counterpart of newTestServer.
func newBenchServer(b *testing.B) (*server, *miniredis.Miniredis) {
	b.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("failed to start miniredis: %v", err)
	}
	b.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	srv := &server{
		rdb:          rdb,
		redisAddr:    mr.Addr(),
		dbClients:    make(map[int]*redis.Client),
		maxBodyBytes: defaultMaxBodyBytes,
	}
	b.Cleanup(srv.Close)
	return srv, mr
}

func BenchmarkV1Set(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	body := strings.Repeat("x", 128)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/string/benchkey", strings.NewReader(body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkV1Get(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/string/benchkey", strings.NewReader("value")))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/string/benchkey", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkLegacySet(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	body := strings.Repeat("x", 128)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/benchkey", strings.NewReader(body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkLegacyGet(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/benchkey", strings.NewReader("value")))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/benchkey", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkV1SetParallel and BenchmarkV1GetParallel drive the handler from
// multiple goroutines (b.RunParallel), profiling throughput under contention
// on the shared router/client rather than single-goroutine latency.
func BenchmarkV1SetParallel(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	body := strings.Repeat("x", 128)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodPost, "/v1/string/benchkey", strings.NewReader(body))
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

func BenchmarkV1GetParallel(b *testing.B) {
	srv, _ := newBenchServer(b)
	h := srv.handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/string/benchkey", strings.NewReader("value")))

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/v1/string/benchkey", nil)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}
