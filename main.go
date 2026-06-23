package main

import (
	"context"
	"crypto/subtle"
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
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/julienschmidt/httprouter"
	"github.com/redis/go-redis/v9"
)

const defaultMaxBodyBytes int64 = 1 << 20 // 1 MiB

// server holds the dependencies and configuration for the HTTP handlers.
type server struct {
	rdb          *redis.Client
	maxBodyBytes int64
	apiToken     string // when empty, authentication is disabled
}

// newRedisClient builds a Redis client from the environment and verifies the
// connection with a bounded Ping.
func newRedisClient() (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     net.JoinHostPort(getEnv("REDIS_HOST", "localhost"), getEnv("REDIS_PORT", "6379")),
		Password: os.Getenv("REDIS_PASSWORD"), // no password by default
		DB:       0,                           // use default DB
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return rdb, nil
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

// setHandler stores the raw request body under the given key, with an optional
// expiration (in seconds) supplied via the `expiration` query parameter.
func (s *server) setHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	key := ps.ByName("key")

	// Bound the body so a single request cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	// Parse the optional expiration parameter.
	var expiration time.Duration
	if expirationParam := r.URL.Query().Get("expiration"); expirationParam != "" {
		exp, err := strconv.Atoi(expirationParam)
		if err != nil || exp < 0 {
			http.Error(w, "Invalid expiration value", http.StatusBadRequest)
			return
		}
		expiration = time.Duration(exp) * time.Second
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

	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	body, err := io.ReadAll(r.Body)
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

// healthHandler reports service readiness by pinging Redis.
func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		http.Error(w, "Redis unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// handler builds the HTTP handler. A small ServeMux fronts httprouter so that
// the static /health route can coexist with the /:key wildcard.
func (s *server) handler() http.Handler {
	router := httprouter.New()
	router.POST("/:key", s.withAuth(s.setHandler))
	router.GET("/:key", s.withAuth(s.getHandler))
	router.DELETE("/:key", s.withAuth(s.deleteHandler))

	// Hash field operations (HSET/HGET/HDEL).
	router.POST("/:key/:field", s.withAuth(s.hsetHandler))
	router.GET("/:key/:field", s.withAuth(s.hgetHandler))
	router.DELETE("/:key/:field", s.withAuth(s.hdelHandler))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.healthHandler)
	mux.Handle("/", router)
	return mux
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	rdb, err := newRedisClient()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer rdb.Close()
	log.Println("Connected to Redis")

	srv := &server{
		rdb:          rdb,
		maxBodyBytes: getEnvInt64("MAX_BODY_BYTES", defaultMaxBodyBytes),
		apiToken:     os.Getenv("API_TOKEN"),
	}
	if srv.apiToken == "" {
		log.Println("WARNING: API_TOKEN is not set — the API is unauthenticated")
	}

	port := getEnv("APP_PORT", "8081")
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
