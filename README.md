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

## **Endpoints**

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
`413 Request Entity Too Large` if the body exceeds `MAX_BODY_BYTES`.

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

| Variable        | Description                                | Default Value |
|------------------|--------------------------------------------|---------------|
| `REDIS_HOST`     | The hostname or IP address of the Redis server. | `localhost`   |
| `REDIS_PORT`     | The Redis server port.                     | `6379`        |
| `REDIS_PASSWORD` | The password for the Redis server (if any). | (empty)       |
| `APP_PORT`       | The port for the REST API server.          | `8081`        |
| `API_TOKEN`      | Bearer token required on key endpoints. If empty, the API is **unauthenticated**. | (empty) |
| `MAX_BODY_BYTES` | Maximum accepted request body size, in bytes. | `1048576` (1 MiB) |

**Example `.env` File**:
```dotenv
REDIS_HOST=redis-server
REDIS_PORT=6379
REDIS_PASSWORD=
APP_PORT=8081
API_TOKEN=
MAX_BODY_BYTES=1048576
```

---

## **Authentication**

When `API_TOKEN` is set, every request to the string and hash endpoints
(`GET`/`POST`/`DELETE` on `/:key` and `/:key/:field`) must include a matching
bearer token:

```bash
curl -H "Authorization: Bearer $API_TOKEN" "http://localhost:8081/mykey"
```

Requests without a valid token receive `401 Unauthorized`. If `API_TOKEN` is left
empty the API accepts all requests and logs a warning at startup. The `/health`
endpoint is always unauthenticated.

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
├── main_test.go            # Unit tests (run with `go test ./...`)
├── go.mod                  # Go module definition
├── go.sum                  # Dependencies checksum
├── Dockerfile              # Dockerfile for containerization
├── e2e/                    # End-to-end test stack (docker compose + run.sh)
├── .github/workflows/      # CI: test, e2e, build & publish image
└── .env                    # Environment variables (not committed to Git)
```

### **Running Tests**
The test suite uses an in-memory Redis ([miniredis](https://github.com/alicebob/miniredis)),
so no running Redis instance is required:
```bash
go test ./...
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
