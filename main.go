package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"
	"github.com/julienschmidt/httprouter"
	"github.com/redis/go-redis/v9"
)

const defaultMaxBodyBytes int64 = 1 << 20 // 1 MiB

// dbHeader selects a non-default logical Redis database on /v1 routes.
const dbHeader = "X-Redis-DB"

// server holds the dependencies and configuration for the HTTP handlers.
type server struct {
	rdb           *redis.Client // DB 0 client; also used by the deprecated flat routes
	redisAddr     string
	redisPassword string
	dbClientsMu   sync.Mutex
	dbClients     map[int]*redis.Client // lazily created clients for non-zero DBs, keyed by DB number

	maxBodyBytes int64
	apiToken     string // when empty, authentication is disabled
}

// newRedisClient builds a Redis client for the given address/password and
// verifies the connection with a bounded Ping.
func newRedisClient(addr, password string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return rdb, nil
}

// clientForDB returns the Redis client for the given logical database,
// creating and caching one on first use for DB numbers other than 0.
func (s *server) clientForDB(db int) *redis.Client {
	if db == 0 {
		return s.rdb
	}
	s.dbClientsMu.Lock()
	defer s.dbClientsMu.Unlock()
	if c, ok := s.dbClients[db]; ok {
		return c
	}
	c := redis.NewClient(&redis.Options{
		Addr:     s.redisAddr,
		Password: s.redisPassword,
		DB:       db,
	})
	s.dbClients[db] = c
	return c
}

// Close closes the default client and any lazily created per-DB clients.
func (s *server) Close() {
	_ = s.rdb.Close()
	s.dbClientsMu.Lock()
	defer s.dbClientsMu.Unlock()
	for _, c := range s.dbClients {
		_ = c.Close()
	}
}

// withAuth wraps a handler so it requires a valid bearer token when an API
// token is configured. With no token configured the API is open (a warning is
// logged at startup).
func (s *server) withAuth(next httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		if s.apiToken != "" && !validToken(r, s.apiToken) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r, ps)
	}
}

// validToken reports whether the request carries the expected bearer token,
// using a constant-time comparison to avoid timing leaks.
func validToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// readBody reads the request body bounded to maxBytes. The returned error, if
// non-nil, is either *http.MaxBytesError (body exceeded the limit) or a
// generic read failure.
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return io.ReadAll(r.Body)
}

// parseExpiration reads the optional "expiration" query parameter (seconds).
func parseExpiration(r *http.Request) (time.Duration, error) {
	expirationParam := r.URL.Query().Get("expiration")
	if expirationParam == "" {
		return 0, nil
	}
	exp, err := strconv.Atoi(expirationParam)
	if err != nil || exp < 0 {
		return 0, errors.New("Invalid expiration value")
	}
	return time.Duration(exp) * time.Second, nil
}

// dbFromRequest reads the optional X-Redis-DB header selecting a non-default
// logical database, defaulting to DB 0.
func dbFromRequest(r *http.Request) (int, error) {
	h := r.Header.Get(dbHeader)
	if h == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(h)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s header %q", dbHeader, h)
	}
	return n, nil
}

// setHandler stores the raw request body under the given key, with an optional
// expiration (in seconds) supplied via the `expiration` query parameter.
func (s *server) setHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	expiration, err := parseExpiration(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.rdb.Set(r.Context(), key, body, expiration).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Key '%s' set successfully", key)
}

// getHandler returns the raw value stored under the given key.
func (s *server) getHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	value, err := s.rdb.Get(r.Context(), key).Bytes()
	if errors.Is(err, redis.Nil) {
		http.Error(w, "Key not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(value)
}

// deleteHandler removes the given key, reporting 404 when it does not exist.
func (s *server) deleteHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	n, err := s.rdb.Del(r.Context(), key).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "Key not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Key '%s' deleted successfully", key)
}

// hsetHandler stores the raw request body as the value of a single field within
// the hash stored at key (Redis HSET).
func (s *server) hsetHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	if err := s.rdb.HSet(r.Context(), key, field, body).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Field '%s' of hash '%s' set successfully", field, key)
}

// hgetHandler returns the raw value of a single hash field (Redis HGET).
func (s *server) hgetHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	value, err := s.rdb.HGet(r.Context(), key, field).Bytes()
	if errors.Is(err, redis.Nil) {
		http.Error(w, "Field not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(value)
}

// hdelHandler removes a single hash field, reporting 404 when it does not exist
// (Redis HDEL).
func (s *server) hdelHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	n, err := s.rdb.HDel(r.Context(), key, field).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "Field not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Field '%s' of hash '%s' deleted successfully", field, key)
}

// errorResponse is the structured JSON body for /v1 error responses.
type errorResponse struct {
	Error string `json:"error"`
}

// statusResponse is the structured JSON body for /v1 write-success responses.
type statusResponse struct {
	Status string `json:"status"`
}

// valueResponse is the structured JSON body for /v1 read responses. Encoding
// is set to "base64" when Value holds base64 rather than raw UTF-8 text.
type valueResponse struct {
	Value    string `json:"value"`
	Encoding string `json:"encoding,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeValue writes a stored value as the response body, honoring content
// negotiation: "Accept: application/octet-stream" returns the raw bytes,
// otherwise a JSON {"value": …} envelope is used (base64-encoded, with an
// "encoding" field, when the value is not valid UTF-8).
func writeValue(w http.ResponseWriter, r *http.Request, value []byte) {
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(value)
		return
	}
	if utf8.Valid(value) {
		writeJSON(w, http.StatusOK, valueResponse{Value: string(value)})
		return
	}
	writeJSON(w, http.StatusOK, valueResponse{Value: base64.StdEncoding.EncodeToString(value), Encoding: "base64"})
}

// setHandlerV1 stores the raw request body under the given key (SET), on the
// database selected by the X-Redis-DB header (default 0).
func (s *server) setHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "Failed to read body")
		return
	}

	expiration, err := parseExpiration(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.clientForDB(db).Set(r.Context(), key, body, expiration).Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// getHandlerV1 returns the value stored under the given key (GET).
func (s *server) getHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	value, err := s.clientForDB(db).Get(r.Context(), key).Bytes()
	if errors.Is(err, redis.Nil) {
		writeJSONError(w, http.StatusNotFound, "Key not found")
		return
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeValue(w, r, value)
}

// deleteHandlerV1 removes the given key (DEL), reporting 404 when absent.
func (s *server) deleteHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	n, err := s.clientForDB(db).Del(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeJSONError(w, http.StatusNotFound, "Key not found")
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// hsetHandlerV1 stores the raw request body as a hash field's value (HSET).
func (s *server) hsetHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "Failed to read body")
		return
	}

	if err := s.clientForDB(db).HSet(r.Context(), key, field, body).Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// hgetHandlerV1 returns a single hash field's value (HGET).
func (s *server) hgetHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	value, err := s.clientForDB(db).HGet(r.Context(), key, field).Bytes()
	if errors.Is(err, redis.Nil) {
		writeJSONError(w, http.StatusNotFound, "Field not found")
		return
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeValue(w, r, value)
}

// hdelHandlerV1 removes a single hash field (HDEL), reporting 404 when absent.
func (s *server) hdelHandlerV1(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")
	field := ps.ByName("field")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	n, err := s.clientForDB(db).HDel(r.Context(), key, field).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeJSONError(w, http.StatusNotFound, "Field not found")
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// notImplementedHandler responds 501 for namespaces reserved by the /v1
// routing foundation but not yet implemented (lists: #6, generic key
// management: #5).
func (s *server) notImplementedHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	writeJSONError(w, http.StatusNotImplemented, "Not implemented yet")
}

// healthHandler reports service readiness by pinging Redis.
func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		http.Error(w, "Redis unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// handler builds the HTTP handler. A ServeMux fronts two separate httprouter
// trees — the deprecated flat routes and the /v1 namespaced routes — so a
// static "/v1" segment never has to coexist with a wildcard at the same tree
// level, and /health can coexist with both.
func (s *server) handler() http.Handler {
	legacy := httprouter.New()
	legacy.POST("/:key", s.withAuth(s.setHandler))
	legacy.GET("/:key", s.withAuth(s.getHandler))
	legacy.DELETE("/:key", s.withAuth(s.deleteHandler))

	// Hash field operations (HSET/HGET/HDEL).
	legacy.POST("/:key/:field", s.withAuth(s.hsetHandler))
	legacy.GET("/:key/:field", s.withAuth(s.hgetHandler))
	legacy.DELETE("/:key/:field", s.withAuth(s.hdelHandler))

	v1 := httprouter.New()
	v1.POST("/v1/string/:key", s.withAuth(s.setHandlerV1))
	v1.GET("/v1/string/:key", s.withAuth(s.getHandlerV1))
	v1.DELETE("/v1/string/:key", s.withAuth(s.deleteHandlerV1))

	v1.POST("/v1/hash/:key/:field", s.withAuth(s.hsetHandlerV1))
	v1.GET("/v1/hash/:key/:field", s.withAuth(s.hgetHandlerV1))
	v1.DELETE("/v1/hash/:key/:field", s.withAuth(s.hdelHandlerV1))

	// Namespaces reserved for future issues (#6 lists, #5 generic key
	// management) — respond 501 until implemented.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		v1.Handle(method, "/v1/list/:key", s.withAuth(s.notImplementedHandler))
		v1.Handle(method, "/v1/keys/:key", s.withAuth(s.notImplementedHandler))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.healthHandler)
	mux.Handle("/v1/", v1)
	mux.Handle("/", legacy)
	return mux
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	redisAddr := net.JoinHostPort(getEnv("REDIS_HOST", "localhost"), getEnv("REDIS_PORT", "6379"))
	redisPassword := os.Getenv("REDIS_PASSWORD") // no password by default

	rdb, err := newRedisClient(redisAddr, redisPassword)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Connected to Redis")

	srv := &server{
		rdb:           rdb,
		redisAddr:     redisAddr,
		redisPassword: redisPassword,
		dbClients:     make(map[int]*redis.Client),
		maxBodyBytes:  getEnvInt64WithLegacy("REDIS_REST_MAX_BODY_BYTES", "MAX_BODY_BYTES", defaultMaxBodyBytes),
		apiToken:      getEnvWithLegacy("REDIS_REST_API_TOKEN", "API_TOKEN", ""),
	}
	defer srv.Close()
	if srv.apiToken == "" {
		log.Println("WARNING: REDIS_REST_API_TOKEN is not set — the API is unauthenticated")
	}

	port := getEnvWithLegacy("REDIS_REST_APP_PORT", "APP_PORT", "8081")
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("Starting server on port %s", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for an interrupt, then shut down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	stop()

	log.Println("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
	}
}

// getEnv returns the value of the environment variable, or fallback if unset.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt64 returns the int64 value of the environment variable, or fallback
// if unset or unparseable.
func getEnvInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Printf("Invalid %s value %q, using default %d", key, v, fallback)
	}
	return fallback
}

// getEnvWithLegacy reads key, falling back to legacyKey (with a deprecation
// warning) for app-specific variables that were previously unprefixed and
// could collide with other services' variables in a shared .env file.
func getEnvWithLegacy(key, legacyKey, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv(legacyKey); v != "" {
		log.Printf("WARNING: %s is deprecated, use %s instead", legacyKey, key)
		return v
	}
	return fallback
}

// getEnvInt64WithLegacy is the int64 counterpart of getEnvWithLegacy.
func getEnvInt64WithLegacy(key, legacyKey string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Printf("Invalid %s value %q, using default %d", key, v, fallback)
		return fallback
	}
	if v := os.Getenv(legacyKey); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			log.Printf("WARNING: %s is deprecated, use %s instead", legacyKey, key)
			return n
		}
		log.Printf("Invalid %s value %q, using default %d", legacyKey, v, fallback)
	}
	return fallback
}
