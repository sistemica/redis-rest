# **redis-rest**

`redis-rest` is a lightweight REST API application that acts as a bridge to interact with a Redis database. It allows clients to perform basic Redis operations (e.g., `GET`, `SET`, `DELETE`) via simple HTTP requests. This is particularly useful for scenarios where RESTful communication is needed to interact with Redis.

---

## **Features**

- Lightweight REST API built with Go.
- Supports basic Redis operations:
  - **SET**: Store raw values with optional expiration.
  - **GET**: Retrieve stored values.
  - **DELETE**: Remove keys from the database.
  - **Hashes**: Set/get/delete individual hash fields (`HSET`/`HGET`/`HDEL`).
- Optional bearer-token authentication.
- Easily configurable via environment variables.
- Dockerized for deployment flexibility.
- Designed to work with Redis running locally, in Docker, or on a remote host.
- Handles raw body input for storing data.

---

## **`/v1` API**

The `/v1` namespace is the current, stable API: explicit type prefixes instead
of segment-count routing, structured JSON responses, and per-request database
selection. New integrations should use this instead of the deprecated flat
routes below.

### **Content negotiation**

Read endpoints (`GET`) return a JSON envelope by default:
```json
{"value": "hello world"}
```
If the stored bytes are not valid UTF-8, `value` is base64-encoded and an
`encoding` field is added:
```json
{"value": "AAH//hA=", "encoding": "base64"}
```
Send `Accept: application/octet-stream` to get the raw bytes back instead
(`Content-Type: application/octet-stream`), exactly as the deprecated flat
routes always have.

Write endpoints (`POST`/`DELETE`) return `{"status": "ok"}` on success. All
errors — auth, validation, missing keys — return `{"error": "..."}` with the
appropriate status code, regardless of the `Accept` header.

### **Database selection**

By default all `/v1` requests use logical database `0`. Send an
`X-Redis-DB: N` header to target another database:
```bash
curl -H "X-Redis-DB: 2" "http://localhost:8081/v1/string/mykey"
```

### **Naming convention: singular vs. plural**

Throughout `/v1`, a **singular** type name (`string`, `hash`) addresses one
specific key/field by path parameter; a **plural** name (`strings`, `hashes`)
addresses bulk/multi-item operations that don't fit that per-item shape.
These are kept as fully separate route trees on purpose: a plural-namespace
operation name can never collide with — or shadow — a real key or field name,
because there is no shared path position between the two trees for it to
collide at.

### **String endpoints**

Single-key operations:

| Method | Path | Redis command |
|--------|------|----------------|
| `POST` | `/v1/string/:key` (`?expiration=<seconds>` optional) | `SET` |
| `GET` | `/v1/string/:key` | `GET` |
| `DELETE` | `/v1/string/:key` | `DEL` |

```bash
curl -X POST "http://localhost:8081/v1/string/mykey?expiration=60" -d "This is my raw value"
curl "http://localhost:8081/v1/string/mykey"
# {"value":"This is my raw value"}
curl -X DELETE "http://localhost:8081/v1/string/mykey"
# {"status":"ok"}
```

Multi-key operations, under `/v1/strings` (plural — a key literally named
`mset` or `mget` is unaffected; it's still reachable at `/v1/string/mset`):

| Method | Path | Redis command |
|--------|------|----------------|
| `POST` | `/v1/strings` | `MSET` |
| `GET` | `/v1/strings?keys=a,b,c` | `MGET` |

`MGET` returns a `values` array in the requested key order, with `null`
entries for keys that don't exist — same shape as hash `HMGET` below.

```bash
curl -X POST "http://localhost:8081/v1/strings" -d '{"values": {"a": "1", "b": "2"}}'
# {"status":"ok"}
curl "http://localhost:8081/v1/strings?keys=a,missing,b"
# {"values":[{"value":"1"},null,{"value":"2"}]}
```

### **Hash endpoints**

Single-field operations, under `/v1/hash`:

| Method | Path | Redis command |
|--------|------|----------------|
| `POST` | `/v1/hash/:key/:field` | `HSET` |
| `GET` | `/v1/hash/:key/:field` | `HGET` |
| `DELETE` | `/v1/hash/:key/:field` | `HDEL` |
| `GET` | `/v1/hash/:key/:field/exists` | `HEXISTS` |
| `POST` | `/v1/hash/:key/:field/incrby` | `HINCRBY` |

```bash
curl -X POST "http://localhost:8081/v1/hash/user1/name" -d "Elvis"
curl "http://localhost:8081/v1/hash/user1/name"
# {"value":"Elvis"}
curl "http://localhost:8081/v1/hash/user1/name/exists"
# {"exists":true}
curl -X POST "http://localhost:8081/v1/hash/counters/views/incrby" -d '{"increment": 5}'
# {"value":5}
```

Whole-hash and multi-field operations, under `/v1/hashes` (plural — a field
literally named `keys` or `values` is unaffected; it's still reachable at
`/v1/hash/:key/keys` via ordinary `HGET`):

| Method | Path | Redis command |
|--------|------|----------------|
| `GET` | `/v1/hashes/:key` | `HGETALL` |
| `POST` | `/v1/hashes/:key` | multi-field `HSET` |
| `GET` | `/v1/hashes/:key/keys` | `HKEYS` |
| `GET` | `/v1/hashes/:key/values` | `HVALS` |
| `GET` | `/v1/hashes/:key/mget?fields=a,b,c` | `HMGET` |
| `GET` | `/v1/hashes/:key/len` | `HLEN` |

`HGETALL` returns a JSON object of field → value envelope; `HMGET` returns a
`values` array in the requested field order, with `null` entries for fields
that don't exist. `HGETALL`/`HKEYS`/`HVALS`/`HLEN` return an empty/zero result
for a missing key rather than an error, matching Redis's own semantics for
those commands.

```bash
curl -X POST "http://localhost:8081/v1/hashes/user1" -d '{"fields": {"name": "Elvis", "last_name": "Presley"}}'
# {"added":2}
curl "http://localhost:8081/v1/hashes/user1"
# {"fields":{"name":{"value":"Elvis"},"last_name":{"value":"Presley"}}}
curl "http://localhost:8081/v1/hashes/user1/mget?fields=name,missing"
# {"values":[{"value":"Elvis"},null]}
```

### **List endpoints**

| Method | Path | Redis command |
|--------|------|----------------|
| `POST` | `/v1/list/:key/left` | `LPUSH` |
| `POST` | `/v1/list/:key/right` | `RPUSH` |
| `DELETE` | `/v1/list/:key/left` (`?count=<n>` optional, default 1) | `LPOP` |
| `DELETE` | `/v1/list/:key/right` (`?count=<n>` optional, default 1) | `RPOP` |
| `GET` | `/v1/list/:key` (`?start=<n>&stop=<n>` optional, default `0`/`-1`) | `LRANGE` |
| `GET` | `/v1/list/:key/len` | `LLEN` |
| `GET` | `/v1/list/:key/index/:index` | `LINDEX` |
| `DELETE` | `/v1/list/:key` | `LREM` |

Push takes a JSON array of string values; pop and range return the same
`{"value": ..., "encoding": ...}` envelope as the string/hash endpoints,
wrapped in a `values` array. `LPOP`/`RPOP` return `404` when the list is
empty or missing (like `GET` on a missing string key); `LRANGE`/`LLEN` return
an empty result instead, matching Redis's own semantics for those commands.

```bash
curl -X POST "http://localhost:8081/v1/list/mylist/right" -d '{"values": ["a", "b"]}'
curl -X POST "http://localhost:8081/v1/list/mylist/left" -d '{"values": ["z"]}'
curl "http://localhost:8081/v1/list/mylist"
# {"values":[{"value":"z"},{"value":"a"},{"value":"b"}]}
curl "http://localhost:8081/v1/list/mylist/len"
# {"length":3}
curl "http://localhost:8081/v1/list/mylist/index/-1"
# {"value":"b"}
curl -X DELETE "http://localhost:8081/v1/list/mylist/left"
# {"values":[{"value":"z"}]}
```

`LREM` takes a JSON body `{"value": "...", "count": <n>}` (`count` optional,
defaults to `0` — remove all occurrences — following Redis `LREM` semantics
directly: positive counts remove from the head, negative from the tail):

```bash
curl -X DELETE "http://localhost:8081/v1/list/mylist" -d '{"value": "a"}'
# {"removed":1}
```

### **Pipeline endpoint (batch, not transactional)**

`POST /v1/pipeline` runs an ordered list of `/v1` requests in a single HTTP
call, to avoid paying a full HTTP round trip per command. Each item is any
other `/v1` route's method + path + optional body, exactly as if you'd called
it directly:

```bash
curl -X POST "http://localhost:8081/v1/pipeline" -d '[
  {"method": "POST", "path": "/v1/string/a", "body": "hello"},
  {"method": "GET", "path": "/v1/string/a"},
  {"method": "POST", "path": "/v1/list/mylist/left", "body": {"values": ["a", "b"]}}
]'
# {"results":[
#   {"status":200,"body":{"status":"ok"}},
#   {"status":200,"body":{"value":"hello"}},
#   {"status":200,"body":{"length":2}}
# ]}
```

`body` is either a JSON string (used verbatim as the raw request body, for
`SET`/`HSET`-style endpoints) or a JSON object/array (used as-is, for
JSON-body endpoints like `LPUSH`/`LREM`/`HINCRBY`/multi-field `HSET`/`MSET`).

**This is a batch, not a transaction.** Items run sequentially, in the order
given, against the same Redis connection context — but each is an
independent request:

- No atomicity: if item 3 of 5 fails, items 1–2 already took effect and are
  **not** rolled back.
- No isolation: another client's commands can interleave between your items.
- A single item's own error (missing key, bad command, invalid path) produces
  a `4xx`/`5xx` result for that item only — the pipeline keeps going and
  still returns `200` overall. Only a malformed *pipeline request itself*
  (invalid JSON, an empty array, more than 100 items) fails the whole call.

If you need real atomicity, Redis's own `MULTI`/`EXEC` (with optional `WATCH`
for optimistic locking) is the right primitive — that's a separate,
not-yet-implemented feature (see `ROADMAP.md`'s `v1.1.0` section), not
something this endpoint provides.

Every item's `Authorization` and `X-Redis-DB` headers are inherited from the
outer pipeline request (so a single token/DB selection covers the whole
batch); each item's result body is always the standard JSON envelope,
regardless of the outer request's `Accept` header.

### **Reserved namespaces**

`/v1/keys/:key` is reserved for generic key management
([#5](../../issues/5)); until that lands it responds `501 Not Implemented`.

---

## **Deprecated flat routes**

> **Deprecated:** these routes predate the `/v1` namespace. They still work
> unchanged (raw bodies in, raw bodies/plain-text out, always DB 0) but new
> integrations should use the [`/v1` API](#v1-api) above.

### **1. Set Key-Value Pair**
**URL**: `POST /:key`

**Description**: Store a key-value pair in Redis, with an optional expiration time.

- **Path Parameter**:
  - `key` (required): The Redis key.
- **Query Parameter**:
  - `expiration` (optional): Expiration time in seconds.
- **Body**: Raw data to store as the value.

**Example**:
```bash
curl -X POST "http://localhost:8081/mykey?expiration=60" \
     -d "This is my raw value"
```

**Response**:
```
HTTP 200 OK
Key 'mykey' set successfully
```

**Errors**: `400 Bad Request` if `expiration` is not a non-negative integer;
`413 Request Entity Too Large` if the body exceeds `REDIS_REST_MAX_BODY_BYTES`.

---

### **2. Get Key**
**URL**: `GET /:key`

**Description**: Retrieve the value of a given key from Redis.

- **Path Parameter**:
  - `key` (required): The Redis key to retrieve.

**Example**:
```bash
curl "http://localhost:8081/mykey"
```

**Response**:
```
HTTP 200 OK
This is my raw value
```

**Errors**: `404 Not Found` if the key does not exist.

---

### **3. Delete Key**
**URL**: `DELETE /:key`

**Description**: Delete a key from Redis.

- **Path Parameter**:
  - `key` (required): The Redis key to delete.

**Example**:
```bash
curl -X DELETE "http://localhost:8081/mykey"
```

**Response**:
```
HTTP 200 OK
Key 'mykey' deleted successfully
```

**Errors**: `404 Not Found` if the key does not exist.

---

## **Hash Endpoints**

Hash fields are addressed as `/:key/:field` (two path segments). These map to the
Redis `HSET`, `HGET`, and `HDEL` commands.

### **4. Set Hash Field**
**URL**: `POST /:key/:field`

**Description**: Set a single field of the hash stored at `key`. The body is the raw value.

```bash
curl -X POST "http://localhost:8081/user1/name" -d "Elvis"
curl -X POST "http://localhost:8081/user1/last_name" -d "Presley"
```

### **5. Get Hash Field**
**URL**: `GET /:key/:field`

**Description**: Retrieve a single field of a hash. Returns `404` if the field (or hash) does not exist.

```bash
curl "http://localhost:8081/user1/name"
# Elvis
```

### **6. Delete Hash Field**
**URL**: `DELETE /:key/:field`

**Description**: Remove a single field from a hash. Returns `404` if the field does not exist.

```bash
curl -X DELETE "http://localhost:8081/user1/name"
curl "http://localhost:8081/user1/name"
# HTTP 404 Field not found
```

> **Note:** single-segment paths (`/:key`) operate on string values; two-segment
> paths (`/:key/:field`) operate on hash fields. Keys and fields containing `/`
> are not supported.

---

## **Environment Variables**

The app uses environment variables for configuration. These variables can be set directly in the runtime environment or passed using a `.env` file.

| Variable                    | Description                                | Default Value |
|------------------------------|--------------------------------------------|---------------|
| `REDIS_HOST`                 | The hostname or IP address of the Redis server. | `localhost`   |
| `REDIS_PORT`                 | The Redis server port.                     | `6379`        |
| `REDIS_PASSWORD`             | The password for the Redis server (if any). | (empty)       |
| `REDIS_REST_APP_PORT`        | The port for the REST API server.          | `8081`        |
| `REDIS_REST_API_TOKEN`       | Bearer token required on key endpoints. If empty, the API is **unauthenticated**. | (empty) |
| `REDIS_REST_MAX_BODY_BYTES`  | Maximum accepted request body size, in bytes. | `1048576` (1 MiB) |

> **Deprecated:** the unprefixed `APP_PORT`, `API_TOKEN`, and `MAX_BODY_BYTES`
> names are still read as a fallback (with a startup warning) so existing
> deployments keep working, but the `REDIS_REST_`-prefixed names above avoid
> collisions when this `.env` is merged into a larger file alongside other
> services (e.g. in `docker compose`). `REDIS_HOST`/`REDIS_PORT`/`REDIS_PASSWORD`
> are left unprefixed since they follow standard Redis client conventions.

**Example `.env` File**:
```dotenv
REDIS_HOST=redis-server
REDIS_PORT=6379
REDIS_PASSWORD=
REDIS_REST_APP_PORT=8081
REDIS_REST_API_TOKEN=
REDIS_REST_MAX_BODY_BYTES=1048576
```

---

## **Authentication**

When `REDIS_REST_API_TOKEN` is set, every request to the string and hash
endpoints (`GET`/`POST`/`DELETE` on `/:key` and `/:key/:field`) must include a
matching bearer token:

```bash
curl -H "Authorization: Bearer $REDIS_REST_API_TOKEN" "http://localhost:8081/mykey"
```

Requests without a valid token receive `401 Unauthorized`. If
`REDIS_REST_API_TOKEN` is left empty the API accepts all requests and logs a
warning at startup. The `/health` endpoint is always unauthenticated.

---

## **Health Check**

**URL**: `GET /health`

Returns `200 OK` when the service can reach Redis, or `503 Service Unavailable`
otherwise. Useful for container/orchestrator liveness and readiness probes.

---

## **Running the App**

### **1. Using Go Directly**
1. **Clone the repository**:
   ```bash
   git clone https://github.com/sistemica/redis-rest.git
   cd redis-rest
   ```

2. **Install dependencies**:
   ```bash
   go mod tidy
   ```

3. **Run the app**:
   ```bash
   go run main.go
   ```

4. **Set environment variables** or provide a `.env` file in the root directory.

---

### **2. Using Docker**

#### **Pulling the Pre-built Image (GHCR)**
The canonical image is published to GitHub Container Registry:
```bash
docker pull ghcr.io/sistemica/redis-rest:latest
```
Available tags: `latest`, `main`, version tags (e.g. `v1.0.0`), and `sha-<commit>`.

> **Deprecated:** the image was previously published as
> `ghcr.io/sistemica/restredis/restredis` (back when this repo was named
> `restredis`). That name is still updated as an alias for backward
> compatibility but is **deprecated** — please switch to
> `ghcr.io/sistemica/redis-rest`.

#### **Building the Docker Image**
```bash
docker build -t redis-rest .
```

#### **Running the Container**
```bash
docker run -d \
  --name redis-rest \
  --env-file .env \
  -p 8081:8081 \
  redis-rest
```

#### **Connecting to Redis**
- If Redis is running in Docker:
  ```bash
  docker network create app-network

  docker run -d \
    --name redis-server \
    --network app-network \
    redis:latest

  docker run -d \
    --name redis-rest \
    --env-file .env \
    --network app-network \
    -p 8081:8081 \
    redis-rest
  ```

- If Redis is running on the host:
  - Use `REDIS_HOST=host.docker.internal` (for Mac/Windows) or the host IP for Linux.

---

## **Development**

### **Requirements**
- Go 1.22 or later
- Redis (local or remote)

### **Directory Structure**
```
.
├── main.go                 # Application entry point
├── main_test.go            # Unit/integration tests (run with `go test ./...`)
├── main_concurrency_test.go # Concurrency tests and throughput benchmarks
├── go.mod                  # Go module definition
├── go.sum                  # Dependencies checksum
├── Dockerfile              # Dockerfile for containerization
├── e2e/                    # End-to-end test stack (docker compose + run.sh)
├── .github/workflows/      # CI: test, e2e, build & publish image
├── LICENSE                 # MIT License
└── .env                    # Environment variables (not committed to Git)
```

### **Running Tests**
The test suite uses an in-memory Redis ([miniredis](https://github.com/alicebob/miniredis)),
so no running Redis instance is required. It covers both the legacy flat
routes and the `/v1` API (unit + integration), plus concurrency and
throughput benchmarks:
```bash
go test ./...              # unit + integration tests
go test -race ./...        # same, with the data race detector
go test -bench=. -run=^$ -benchmem ./...   # throughput benchmarks only
```

To profile a benchmark:
```bash
go test -bench=BenchmarkV1Get -run=^$ -cpuprofile=cpu.out -memprofile=mem.out .
go tool pprof cpu.out
```

### **End-to-End Tests**
`e2e/run.sh` builds the API image, starts it alongside a Redis-compatible
datastore (Valkey by default) via docker compose, runs every scenario against
the live stack, and tears it down:
```bash
./e2e/run.sh                            # against Valkey (BSD-licensed Redis fork)
REDIS_IMAGE=redis:7-alpine ./e2e/run.sh # against Redis
HOST_PORT=18081 ./e2e/run.sh            # if 8081 is taken
```

### **Testing the Endpoints**
- Use `curl`, Postman, or any REST client to interact with the API.

---

## **Troubleshooting**

### **Redis Connection Issues**
1. Verify Redis is running and reachable:
   ```bash
   redis-cli -h <REDIS_HOST> -p <REDIS_PORT> ping
   ```
   Expected output:
   ```
   PONG
   ```

2. Check the `REDIS_HOST` and `REDIS_PORT` environment variables.

### **Docker-Specific Issues**
- If using `host.docker.internal` and it fails:
  - Use the host's IP address (`172.17.0.1` for Linux) or Docker's host networking mode (`--network host`).

---

## **Extending the App**

### **Additional Features**
- **Additional Redis Commands**:
  - Support for more commands like `EXISTS`, `INCR`, `HGETALL`, etc.
- **Monitoring**:
  - Integrate with monitoring tools like Prometheus for metrics.
- **WebSocket Support**:
  - Add real-time updates for subscribed keys.

---

## **License**
This project is licensed under the [MIT License](LICENSE).

---

Feel free to reach out for suggestions or contributions! 🚀
