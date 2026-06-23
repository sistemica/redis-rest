# Roadmap

This document tracks the path from the current minimal API to a credible
`1.0.0` and beyond.

## Current state (`v0.x`)

Implemented today:

- **Strings:** `SET` (with optional expiration), `GET`, `DEL`
- **Hashes:** `HSET`, `HGET`, `HDEL`
- Optional bearer-token auth, `/health` readiness check, body-size limits,
  graceful shutdown
- Routing overloads paths by segment count: `/:key` (string), `/:key/:field`
  (hash)

This is enough to be useful but too thin to call "Redis over REST".

---

## `v1.0.0` — a credible Redis REST wrapper

**Goal:** cover the common operations of the core data types plus the
key-management essentials, behind a stable, versioned API.

### Routing & response redesign (breaking — do first)

The current segment-count routing will collide as more types are added. `1.0`
introduces an explicit, versioned namespace and structured responses.

- [ ] Version prefix: `/v1/...`
- [ ] Explicit type namespaces: `/v1/string/:key`, `/v1/hash/:key`,
      `/v1/list/:key`, `/v1/keys/:key/...`
- [ ] Consistent JSON responses (`{"value": …}` / `{"error": …}`) with
      content negotiation for raw bodies
- [ ] Database selection (`?db=N` or header) instead of hardcoded DB 0

### Complete the existing types

- [ ] **Strings:** `INCR`, `DECR`, `INCRBY`, `DECRBY`, `APPEND`, `SETNX`,
      `GETSET`, `MGET`, `MSET`, `GETEX`
- [ ] **Hashes:** `HGETALL`, `HKEYS`, `HVALS`, `HMGET`, multi-field `HSET`,
      `HEXISTS`, `HLEN`, `HINCRBY`

### Key / generic management (the biggest current gap)

- [ ] `EXISTS`, `TYPE`, `RENAME`
- [ ] `EXPIRE` / `PEXPIRE`, `TTL` / `PTTL`, `PERSIST`
- [ ] `SCAN` (cursor-based iteration — never `KEYS *` over HTTP)
- [ ] Multi-key `DEL`

### Lists

- [ ] `LPUSH` / `RPUSH`, `LPOP` / `RPOP`, `LRANGE`, `LLEN`, `LREM`, `LINDEX`

### Cross-cutting

- [ ] Batch / pipeline endpoint (`/v1/pipeline`) to avoid per-command HTTP cost
- [ ] OpenAPI spec + generated docs
- [ ] README "supported commands" matrix so coverage is never misleading

---

## `v1.1.0` and later

- **Sets:** `SADD`, `SREM`, `SMEMBERS`, `SISMEMBER`, `SCARD`, `SINTER` /
  `SUNION` / `SDIFF`
- **Sorted sets:** `ZADD`, `ZRANGE`, `ZRANGEBYSCORE`, `ZSCORE`, `ZRANK`,
  `ZREM`, `ZINCRBY`
- **Transactions:** `MULTI` / `EXEC`

---

## Deferred (not required for `1.0`)

- Pub/Sub (`PUBLISH` / `SUBSCRIBE`) — needs WebSocket/SSE, a different transport
- Streams (`XADD` / `XREAD`)
- Server-side scripting (`EVAL`)
- Admin commands (`INFO`, `FLUSHDB`, `CONFIG`)
