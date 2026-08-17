package main

import (
	"bytes"
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

// withAuth wraps an httprouter.Handle (used by the deprecated flat routes) so
// it requires a valid bearer token when an API token is configured. With no
// token configured the API is open (a warning is logged at startup).
func (s *server) withAuth(next httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		if s.apiToken != "" && !validToken(r, s.apiToken) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r, ps)
	}
}

// withAuthFunc is the http.HandlerFunc counterpart of withAuth, used by the
// /v1 routes (served from a native ServeMux rather than httprouter).
func (s *server) withAuthFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" && !validToken(r, s.apiToken) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
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

// toValueResponse encodes a stored value as a valueResponse, base64-encoding
// it (and setting Encoding) when it is not valid UTF-8.
func toValueResponse(value []byte) valueResponse {
	if utf8.Valid(value) {
		return valueResponse{Value: string(value)}
	}
	return valueResponse{Value: base64.StdEncoding.EncodeToString(value), Encoding: "base64"}
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
	writeJSON(w, http.StatusOK, toValueResponse(value))
}

// setHandlerV1 stores the raw request body under the given key (SET), on the
// database selected by the X-Redis-DB header (default 0).
func (s *server) setHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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
func (s *server) getHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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
func (s *server) deleteHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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

// msetRequest is the JSON request body for MSET.
type msetRequest struct {
	Values map[string]string `json:"values"`
}

// msetHandlerV1 sets several string keys atomically (MSET) per the JSON
// request body {"values": {"key1": "value1", ...}}.
func (s *server) msetHandlerV1(w http.ResponseWriter, r *http.Request) {
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

	var req msetRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Values) == 0 {
		writeJSONError(w, http.StatusBadRequest, `Request body must be JSON: {"values": {"key": "value"}}`)
		return
	}

	args := make([]any, 0, len(req.Values)*2)
	for key, value := range req.Values {
		args = append(args, key, value)
	}

	if err := s.clientForDB(db).MSet(r.Context(), args...).Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

// mgetHandlerV1 returns the values of several string keys (MGET), given as a
// comma-separated ?keys=a,b,c query parameter. The result preserves the
// requested order, with null entries for keys that don't exist.
func (s *server) mgetHandlerV1(w http.ResponseWriter, r *http.Request) {
	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	keysParam := r.URL.Query().Get("keys")
	if keysParam == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing required ?keys=a,b,c query parameter")
		return
	}
	keys := strings.Split(keysParam, ",")

	values, err := s.clientForDB(db).MGet(r.Context(), keys...).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]*valueResponse, len(values))
	for i, v := range values {
		str, ok := v.(string)
		if !ok {
			continue // nil: key doesn't exist, leave as null
		}
		vr := toValueResponse([]byte(str))
		out[i] = &vr
	}
	writeJSON(w, http.StatusOK, nullableValuesResponse{Values: out})
}

// hsetHandlerV1 stores the raw request body as a hash field's value (HSET).
func (s *server) hsetHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	field := r.PathValue("field")

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
func (s *server) hgetHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	field := r.PathValue("field")

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
func (s *server) hdelHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	field := r.PathValue("field")

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

// hashFieldsResponse is the structured JSON body for HGETALL responses.
type hashFieldsResponse struct {
	Fields map[string]valueResponse `json:"fields"`
}

// keysResponse is the structured JSON body for HKEYS responses.
type keysResponse struct {
	Keys []string `json:"keys"`
}

// nullableValuesResponse is the structured JSON body for HMGET responses;
// entries are null for fields that don't exist, matching Redis HMGET.
type nullableValuesResponse struct {
	Values []*valueResponse `json:"values"`
}

// addedResponse is the structured JSON body for multi-field HSET responses.
type addedResponse struct {
	Added int64 `json:"added"`
}

// existsResponse is the structured JSON body for HEXISTS responses.
type existsResponse struct {
	Exists bool `json:"exists"`
}

// multiSetRequest is the JSON request body for multi-field HSET.
type multiSetRequest struct {
	Fields map[string]string `json:"fields"`
}

// incrByRequest is the JSON request body for HINCRBY.
type incrByRequest struct {
	Increment int64 `json:"increment"`
}

// incrByResponse is the structured JSON body for HINCRBY responses.
type incrByResponse struct {
	Value int64 `json:"value"`
}

// hgetallHandlerV1 returns every field/value pair in the hash (HGETALL).
func (s *server) hgetallHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	fields, err := s.clientForDB(db).HGetAll(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make(map[string]valueResponse, len(fields))
	for field, value := range fields {
		out[field] = toValueResponse([]byte(value))
	}
	writeJSON(w, http.StatusOK, hashFieldsResponse{Fields: out})
}

// hmsetHandlerV1 sets several hash fields in one request (multi-field HSET)
// per the JSON request body {"fields": {"name": "value", ...}}.
func (s *server) hmsetHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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

	var req multiSetRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Fields) == 0 {
		writeJSONError(w, http.StatusBadRequest, `Request body must be JSON: {"fields": {"name": "value"}}`)
		return
	}

	args := make([]any, 0, len(req.Fields)*2)
	for field, value := range req.Fields {
		args = append(args, field, value)
	}

	added, err := s.clientForDB(db).HSet(r.Context(), key, args...).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, addedResponse{Added: added})
}

// hkeysHandlerV1 returns every field name in the hash (HKEYS).
func (s *server) hkeysHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	keys, err := s.clientForDB(db).HKeys(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, keysResponse{Keys: keys})
}

// hvalsHandlerV1 returns every field value in the hash, unordered relative to
// field names (HVALS).
func (s *server) hvalsHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	values, err := s.clientForDB(db).HVals(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, valuesResponse{Values: toValueResponses(values)})
}

// hmgetHandlerV1 returns the values of several fields (HMGET), given as a
// comma-separated ?fields=a,b,c query parameter. The result preserves the
// requested order, with null entries for fields that don't exist.
func (s *server) hmgetHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	fieldsParam := r.URL.Query().Get("fields")
	if fieldsParam == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing required ?fields=a,b,c query parameter")
		return
	}
	fields := strings.Split(fieldsParam, ",")

	values, err := s.clientForDB(db).HMGet(r.Context(), key, fields...).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]*valueResponse, len(values))
	for i, v := range values {
		s, ok := v.(string)
		if !ok {
			continue // nil: field doesn't exist, leave as null
		}
		vr := toValueResponse([]byte(s))
		out[i] = &vr
	}
	writeJSON(w, http.StatusOK, nullableValuesResponse{Values: out})
}

// hlenHandlerV1 returns the number of fields in the hash (0 for a missing
// key, matching Redis HLEN semantics — no error).
func (s *server) hlenHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	length, err := s.clientForDB(db).HLen(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, lengthResponse{Length: length})
}

// hexistsHandlerV1 reports whether a hash field exists (HEXISTS).
func (s *server) hexistsHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	field := r.PathValue("field")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	exists, err := s.clientForDB(db).HExists(r.Context(), key, field).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, existsResponse{Exists: exists})
}

// hincrbyHandlerV1 atomically increments (or decrements, for a negative
// value) a hash field by the JSON request body {"increment": N} (HINCRBY),
// creating the field (from 0) if it doesn't already exist.
func (s *server) hincrbyHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	field := r.PathValue("field")

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

	var req incrByRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, `Request body must be JSON: {"increment": N}`)
		return
	}

	value, err := s.clientForDB(db).HIncrBy(r.Context(), key, field, req.Increment).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, incrByResponse{Value: value})
}

// lengthResponse is the structured JSON body for LLEN/LPUSH/RPUSH/HLEN
// responses.
type lengthResponse struct {
	Length int64 `json:"length"`
}

// valuesResponse is the structured JSON body for LRANGE/LPOP/RPOP/HVALS
// responses.
type valuesResponse struct {
	Values []valueResponse `json:"values"`
}

// removedResponse is the structured JSON body for LREM responses.
type removedResponse struct {
	Removed int64 `json:"removed"`
}

// pushRequest is the JSON request body for LPUSH/RPUSH.
type pushRequest struct {
	Values []string `json:"values"`
}

// remRequest is the JSON request body for LREM. Count follows Redis LREM
// semantics directly: >0 removes that many from the head, <0 from the tail,
// 0 (the Go zero value, so it need not be supplied) removes all occurrences.
type remRequest struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

func toValueResponses(values []string) []valueResponse {
	out := make([]valueResponse, len(values))
	for i, v := range values {
		out[i] = toValueResponse([]byte(v))
	}
	return out
}

// listPushHandlerV1 returns a handler that LPUSHes or RPUSHes (per side) the
// JSON-encoded values in the request body: {"values": ["a", "b"]}.
func (s *server) listPushHandlerV1(side string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")

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

		var req pushRequest
		if err := json.Unmarshal(body, &req); err != nil || len(req.Values) == 0 {
			writeJSONError(w, http.StatusBadRequest, `Request body must be JSON: {"values": ["..."]}`)
			return
		}

		args := make([]any, len(req.Values))
		for i, v := range req.Values {
			args[i] = v
		}

		client := s.clientForDB(db)
		var length int64
		if side == "left" {
			length, err = client.LPush(r.Context(), key, args...).Result()
		} else {
			length, err = client.RPush(r.Context(), key, args...).Result()
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, lengthResponse{Length: length})
	}
}

// listPopHandlerV1 returns a handler that LPOPs or RPOPs (per side) up to
// `count` values (default 1, via the optional ?count= query parameter).
// Reports 404 when the list is empty or missing, matching the string/hash GET
// endpoints' "not found" behavior.
func (s *server) listPopHandlerV1(side string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")

		db, err := dbFromRequest(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		count := 1
		if countParam := r.URL.Query().Get("count"); countParam != "" {
			n, err := strconv.Atoi(countParam)
			if err != nil || n < 1 {
				writeJSONError(w, http.StatusBadRequest, "Invalid count value")
				return
			}
			count = n
		}

		client := s.clientForDB(db)
		var values []string
		if side == "left" {
			values, err = client.LPopCount(r.Context(), key, count).Result()
		} else {
			values, err = client.RPopCount(r.Context(), key, count).Result()
		}
		if errors.Is(err, redis.Nil) {
			writeJSONError(w, http.StatusNotFound, "Key not found")
			return
		} else if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, valuesResponse{Values: toValueResponses(values)})
	}
}

// listRangeHandlerV1 returns the elements of a list between the optional
// ?start= and ?stop= query parameters (default 0 and -1, i.e. the whole
// list), matching Redis LRANGE index semantics including negative indices.
func (s *server) listRangeHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	start, stop := int64(0), int64(-1)
	if v := r.URL.Query().Get("start"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid start value")
			return
		}
		start = n
	}
	if v := r.URL.Query().Get("stop"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid stop value")
			return
		}
		stop = n
	}

	values, err := s.clientForDB(db).LRange(r.Context(), key, start, stop).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, valuesResponse{Values: toValueResponses(values)})
}

// listLenHandlerV1 returns a list's length (0 for a missing key, matching
// Redis LLEN semantics — no error).
func (s *server) listLenHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	length, err := s.clientForDB(db).LLen(r.Context(), key).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, lengthResponse{Length: length})
}

// listIndexHandlerV1 returns the element at the given (possibly negative)
// index, reporting 404 when the index is out of range or the key is missing.
func (s *server) listIndexHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	db, err := dbFromRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	index, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid index value")
		return
	}

	value, err := s.clientForDB(db).LIndex(r.Context(), key, index).Bytes()
	if errors.Is(err, redis.Nil) {
		writeJSONError(w, http.StatusNotFound, "Index out of range")
		return
	} else if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeValue(w, r, value)
}

// listRemHandlerV1 removes occurrences of a value (LREM) per the JSON request
// body {"value": "...", "count": N}. Always succeeds, even removing zero
// elements, matching Redis LREM semantics (no error for a missing key).
func (s *server) listRemHandlerV1(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

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

	var req remRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, `Request body must be JSON: {"value": "...", "count": 0}`)
		return
	}

	removed, err := s.clientForDB(db).LRem(r.Context(), key, req.Count, req.Value).Result()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, removedResponse{Removed: removed})
}

// notImplementedHandler responds 501 for namespaces reserved by the /v1
// routing foundation but not yet implemented (generic key management: #5).
func (s *server) notImplementedHandler(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusNotImplemented, "Not implemented yet")
}

// maxPipelineItems bounds a single /v1/pipeline request to keep worst-case
// latency and memory bounded; the overall request body is already bounded by
// maxBodyBytes.
const maxPipelineItems = 100

// pipelineItem is one element of a /v1/pipeline request body: an HTTP method
// and path matching any other /v1 route, plus an optional body — exactly as
// if the client had made that request directly. Body may be a JSON string
// (used verbatim as the raw request body, matching the SET/HSET-style
// endpoints) or a JSON object/array (re-encoded and used as-is, matching the
// JSON-body endpoints like LPUSH/LREM/HINCRBY/multi-HSET/MSET).
type pipelineItem struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// pipelineResultItem is the response counterpart to pipelineItem: the status
// and JSON body the underlying route would have returned to a direct caller.
type pipelineResultItem struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// pipelineResponse is the structured JSON body for /v1/pipeline responses.
type pipelineResponse struct {
	Results []pipelineResultItem `json:"results"`
}

// responseCapture is a minimal http.ResponseWriter that records a response
// instead of writing it to a network connection, used by pipelineHandlerV1 to
// run a /v1 route internally without a real request/response round trip.
type responseCapture struct {
	status int
	body   bytes.Buffer
	header http.Header
}

func newResponseCapture() *responseCapture {
	return &responseCapture{status: http.StatusOK, header: make(http.Header)}
}

func (rc *responseCapture) Header() http.Header         { return rc.header }
func (rc *responseCapture) Write(b []byte) (int, error) { return rc.body.Write(b) }
func (rc *responseCapture) WriteHeader(status int)      { rc.status = status }

func pipelineErrorResult(status int, msg string) pipelineResultItem {
	b, _ := json.Marshal(errorResponse{Error: msg})
	return pipelineResultItem{Status: status, Body: b}
}

// pipelineHandlerV1 executes an ordered list of /v1 requests in one HTTP
// call, avoiding a per-command round trip (#7). Each item runs independently
// and sequentially in request order — this is deliberately NOT a
// transaction: there is no atomicity or isolation. If item 3 of 5 fails,
// items 1-2 already took effect and are not rolled back, and other clients'
// commands may interleave between items. An invalid item (bad method, a path
// outside /v1, or /v1/pipeline itself) produces a per-item 400 result rather
// than failing the whole batch.
func (s *server) pipelineHandlerV1(w http.ResponseWriter, r *http.Request) {
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

	var items []pipelineItem
	if err := json.Unmarshal(body, &items); err != nil {
		writeJSONError(w, http.StatusBadRequest, `Request body must be a JSON array: [{"method": "...", "path": "...", "body": ...}]`)
		return
	}
	if len(items) == 0 {
		writeJSONError(w, http.StatusBadRequest, "Pipeline must contain at least one item")
		return
	}
	if len(items) > maxPipelineItems {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Pipeline exceeds the %d item limit", maxPipelineItems))
		return
	}

	v1 := s.v1Mux()
	results := make([]pipelineResultItem, len(items))
	for i, item := range items {
		results[i] = s.runPipelineItem(v1, r, item)
	}

	writeJSON(w, http.StatusOK, pipelineResponse{Results: results})
}

// runPipelineItem validates and dispatches a single pipeline item through the
// v1 mux, forwarding the outer request's auth and DB-selection headers (but
// not its Accept header — pipeline item results are always the standard JSON
// envelope, never raw octet-stream), and captures the resulting status and
// body.
func (s *server) runPipelineItem(v1 *http.ServeMux, outer *http.Request, item pipelineItem) pipelineResultItem {
	method := strings.ToUpper(item.Method)
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		return pipelineErrorResult(http.StatusBadRequest, fmt.Sprintf("Unsupported method %q", item.Method))
	}
	if !strings.HasPrefix(item.Path, "/v1/") || item.Path == "/v1/pipeline" {
		return pipelineErrorResult(http.StatusBadRequest, fmt.Sprintf("Invalid path %q: must be a /v1/... route other than /v1/pipeline", item.Path))
	}

	var reqBody io.Reader
	if len(item.Body) > 0 && string(item.Body) != "null" {
		var raw string
		if err := json.Unmarshal(item.Body, &raw); err == nil {
			reqBody = strings.NewReader(raw)
		} else {
			reqBody = bytes.NewReader(item.Body)
		}
	}

	subReq, err := http.NewRequestWithContext(outer.Context(), method, item.Path, reqBody)
	if err != nil {
		return pipelineErrorResult(http.StatusBadRequest, fmt.Sprintf("Invalid path %q: %v", item.Path, err))
	}
	subReq.Header.Set("Authorization", outer.Header.Get("Authorization"))
	if db := outer.Header.Get(dbHeader); db != "" {
		subReq.Header.Set(dbHeader, db)
	}

	rc := newResponseCapture()
	v1.ServeHTTP(rc, subReq)

	respBody := rc.body.Bytes()
	if len(respBody) == 0 {
		respBody = []byte("null")
	}
	return pipelineResultItem{Status: rc.status, Body: respBody}
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

// v1Mux builds the /v1 ServeMux (Go 1.22+ path wildcards).
//
// Bulk/multi-item operations (MSET/MGET, HGETALL/HKEYS/HVALS/HMGET/HLEN/
// multi-field HSET) live under their own plural namespace ("/v1/strings",
// "/v1/hashes") rather than as literal siblings of a {key}/{field} wildcard
// in the singular one ("/v1/string", "/v1/hash"). This is deliberate: a
// sibling literal like "keys" would take routing precedence over the
// wildcard for that exact path, silently shadowing any real key or field
// that happened to be named "keys". Giving bulk operations a separate tree
// with no competing wildcard makes that shadowing structurally impossible,
// not just unlikely, and keeps every command's path unambiguous regardless
// of what data actually exists.
//
// Also used directly by pipelineHandlerV1 (see below) to dispatch each
// batched item through the exact same routing and auth logic a top-level
// request would go through — a fresh instance per pipeline call, same as
// handler() itself rebuilds its trees per call.
func (s *server) v1Mux() *http.ServeMux {
	v1 := http.NewServeMux()

	v1.HandleFunc("POST /v1/string/{key}", s.withAuthFunc(s.setHandlerV1))
	v1.HandleFunc("GET /v1/string/{key}", s.withAuthFunc(s.getHandlerV1))
	v1.HandleFunc("DELETE /v1/string/{key}", s.withAuthFunc(s.deleteHandlerV1))

	// Strings: multi-key operations (MSET/MGET) live under the plural
	// "/v1/strings" namespace — a tree with no {key} wildcard at all — so a
	// real key can never be shadowed by these, unlike a same-tree literal
	// sibling would risk.
	v1.HandleFunc("POST /v1/strings", s.withAuthFunc(s.msetHandlerV1))
	v1.HandleFunc("GET /v1/strings", s.withAuthFunc(s.mgetHandlerV1))

	// Hash: single-field operations (HSET/HGET/HDEL/HEXISTS/HINCRBY).
	v1.HandleFunc("POST /v1/hash/{key}/{field}", s.withAuthFunc(s.hsetHandlerV1))
	v1.HandleFunc("GET /v1/hash/{key}/{field}", s.withAuthFunc(s.hgetHandlerV1))
	v1.HandleFunc("DELETE /v1/hash/{key}/{field}", s.withAuthFunc(s.hdelHandlerV1))
	v1.HandleFunc("GET /v1/hash/{key}/{field}/exists", s.withAuthFunc(s.hexistsHandlerV1))
	v1.HandleFunc("POST /v1/hash/{key}/{field}/incrby", s.withAuthFunc(s.hincrbyHandlerV1))

	// Hashes: whole-hash and multi-field operations, under the plural
	// "/v1/hashes" namespace — a separate tree from "/v1/hash", so a hash
	// field can never be shadowed by e.g. a field literally named "keys".
	v1.HandleFunc("GET /v1/hashes/{key}", s.withAuthFunc(s.hgetallHandlerV1))
	v1.HandleFunc("POST /v1/hashes/{key}", s.withAuthFunc(s.hmsetHandlerV1))
	v1.HandleFunc("GET /v1/hashes/{key}/keys", s.withAuthFunc(s.hkeysHandlerV1))
	v1.HandleFunc("GET /v1/hashes/{key}/values", s.withAuthFunc(s.hvalsHandlerV1))
	v1.HandleFunc("GET /v1/hashes/{key}/mget", s.withAuthFunc(s.hmgetHandlerV1))
	v1.HandleFunc("GET /v1/hashes/{key}/len", s.withAuthFunc(s.hlenHandlerV1))

	// List operations (LPUSH/RPUSH/LPOP/RPOP/LRANGE/LLEN/LREM/LINDEX).
	v1.HandleFunc("GET /v1/list/{key}", s.withAuthFunc(s.listRangeHandlerV1))
	v1.HandleFunc("DELETE /v1/list/{key}", s.withAuthFunc(s.listRemHandlerV1))
	v1.HandleFunc("GET /v1/list/{key}/len", s.withAuthFunc(s.listLenHandlerV1))
	v1.HandleFunc("GET /v1/list/{key}/index/{index}", s.withAuthFunc(s.listIndexHandlerV1))
	v1.HandleFunc("POST /v1/list/{key}/left", s.withAuthFunc(s.listPushHandlerV1("left")))
	v1.HandleFunc("POST /v1/list/{key}/right", s.withAuthFunc(s.listPushHandlerV1("right")))
	v1.HandleFunc("DELETE /v1/list/{key}/left", s.withAuthFunc(s.listPopHandlerV1("left")))
	v1.HandleFunc("DELETE /v1/list/{key}/right", s.withAuthFunc(s.listPopHandlerV1("right")))

	// Batch endpoint: run several /v1 requests in one HTTP call (#7).
	v1.HandleFunc("POST /v1/pipeline", s.withAuthFunc(s.pipelineHandlerV1))

	// Namespace reserved for #5 (generic key management) — respond 501 until
	// implemented.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		v1.HandleFunc(method+" /v1/keys/{key}", s.withAuthFunc(s.notImplementedHandler))
	}

	return v1
}

// handler builds the HTTP handler. The deprecated flat routes are served
// from an httprouter tree; /v1 is served from v1Mux's native ServeMux.
func (s *server) handler() http.Handler {
	legacy := httprouter.New()
	legacy.POST("/:key", s.withAuth(s.setHandler))
	legacy.GET("/:key", s.withAuth(s.getHandler))
	legacy.DELETE("/:key", s.withAuth(s.deleteHandler))

	// Hash field operations (HSET/HGET/HDEL).
	legacy.POST("/:key/:field", s.withAuth(s.hsetHandler))
	legacy.GET("/:key/:field", s.withAuth(s.hgetHandler))
	legacy.DELETE("/:key/:field", s.withAuth(s.hdelHandler))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.healthHandler)
	mux.Handle("/v1/", s.v1Mux())
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
