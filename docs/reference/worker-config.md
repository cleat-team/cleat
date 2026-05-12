# cleat-worker Configuration Reference

The `cleat-worker` is a production worker daemon that polls the database for
runnable workflow instances, loads WASM modules, replays event history, and
drives execution. It supports PostgreSQL, MySQL, and SQL Server backends.

    cleat-worker [flags]

---

## Core flags

### --db

| Type | Default | Env var |
|------|---------|---------|
| string | `""` (required) | `DATABASE_URL` |

PostgreSQL (or MySQL/SQL Server) connection URL. The worker connects to your
existing database; it does not manage it. Example:

    --db "postgres://user:pass@localhost:5432/cleat?sslmode=disable"
    --db "mysql://user:pass@tcp(localhost:3306)/cleat"
    --db "sqlserver://user:pass@localhost:1433?database=cleat"

---

### --schema

| Type | Default | Env var |
|------|---------|---------|
| string | `"public"` | -- |

PostgreSQL schema for cleat tables. Sets `search_path` on connections; runs
`CREATE SCHEMA IF NOT EXISTS` on startup. Enables multiple isolated worker
pools on a single database cluster.

---

### --api-addr

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | HTTP API listen address (e.g. `:8080`) |

When set, the worker starts an HTTP server with endpoints for workflow
management, schedules, definitions, signals, promises, health checks, an admin
drain API, and Prometheus metrics (`/metrics`). An embedded Svelte web UI is
served at the root path.

---

## Concurrency

### --concurrency

| Type | Default | Description |
|------|---------|-------------|
| int | `10` | Maximum concurrent workflow executions per worker |

Controls how many workflows a single worker process can execute in parallel.
Used alongside memory-aware dynamic concurrency when `--memory-soft-limit` is
configured.

---

### --task-queue

| Type | Default | Description |
|------|---------|-------------|
| string | `"default"` | Comma-separated task queues to poll |

A worker can poll multiple queues. Example:

    --task-queue "default,gpu,high-memory"

---

## Timing

### --heartbeat

| Type | Default | Description |
|------|---------|-------------|
| duration | `5s` | Heartbeat interval |

The worker updates its heartbeat in the database at this interval. Stale
instances (missing two consecutive heartbeats) are reaped and made available
to other workers.

---

### --poll

| Type | Default | Description |
|------|---------|-------------|
| duration | `500ms` | Poll interval when no work is found |

How long the worker waits between dispatch-loop iterations when no runnable
workflows are found. Uses progressive backoff up to 6x this value.

---

## Memory Management

### --memory-soft-limit

| Type | Default | Description |
|------|---------|-------------|
| float | `0.80` | Memory soft limit fraction (0.0-1.0) |

When system memory usage exceeds this fraction, the worker stops claiming new
work from the database but continues executing in-flight workflows.

---

### --memory-hard-limit

| Type | Default | Description |
|------|---------|-------------|
| float | `0.95` | Memory hard limit fraction (0.0-1.0) |

When system memory exceeds this fraction, the worker rejects API-initiated
workflow starts (returns HTTP 503).

---

### --memory-check-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `2s` | Interval between memory readings |

---

### --memory-sample-retention

| Type | Default | Description |
|------|---------|-------------|
| int | `1000` | Max memory samples per workflow definition |

---

## Authentication

### --require-auth

| Type | Default | Description |
|------|---------|-------------|
| bool | `true` | Require API key authentication for HTTP API |

When enabled, all API routes require a `Authorization: Bearer <key>` header.
Auto-generates an API key on first startup if no keys exist.

---

### --tenant-resolver

| Type | Default | Description |
|------|---------|-------------|
| string | `"single-tenant"` | Tenant resolution mode |

Modes:

| Mode | Description |
|------|-------------|
| `single-tenant` | No tenant extraction; all workflows use the default tenant |
| `header:<name>` | Extract tenant ID from the named HTTP header (e.g. `header:X-Tenant-ID`) |
| `api-key` | Resolve tenant from the API key used for authentication |

---

## Plugins

### --plugin-config

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to a plugin configuration JSON file |

The file contents are expanded with `os.ExpandEnv` before parsing.

---

## Compaction

### --compaction-threshold

| Type | Default | Description |
|------|---------|-------------|
| int | (host default) | Number of events before history compaction triggers |

Compaction collapses event history for long-running workflows, retaining only
the compacted state.

---

### --compaction-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `5m` | Interval between compaction checks |

---

## Networking

### --max-body-size

| Type | Default | Description |
|------|---------|-------------|
| int64 | `1048576` (1 MiB) | Maximum request body size in bytes |

General endpoints use this limit. Signal endpoints have a fixed 64 KB limit.

---

### --http-read-timeout

| Type | Default | Description |
|------|---------|-------------|
| duration | `30s` | HTTP read timeout |

---

### --http-write-timeout

| Type | Default | Description |
|------|---------|-------------|
| duration | `60s` | HTTP write timeout |

---

### --http-idle-timeout

| Type | Default | Description |
|------|---------|-------------|
| duration | `120s` | HTTP idle timeout |

---

### --rate-limit

| Type | Default | Description |
|------|---------|-------------|
| float | `100` | Requests/second/IP rate limit (only when `--api-addr` is set) |

---

### --rate-limit-burst

| Type | Default | Description |
|------|---------|-------------|
| int | `200` | Rate limit burst size |

---

## WASM Cache

### --wasm-cache-max-entries

| Type | Default | Description |
|------|---------|-------------|
| int | `100` | Max WASM byte cache entries (LRU eviction) |

---

### --wasm-cache-max-mb

| Type | Default | Description |
|------|---------|-------------|
| int | `500` | Max WASM byte cache total size in MB (LRU eviction) |

---

## Data Management

### --retention-days

| Type | Default | Description |
|------|---------|-------------|
| int | `30` | Days to retain completed/failed workflow event history (0 disables) |

---

### --redact

| Type | Default | Description |
|------|---------|-------------|
| bool | `true` | Enable redaction of sensitive fields in event history |

---

### --max-retries

| Type | Default | Description |
|------|---------|-------------|
| int | `100` | Maximum retry attempts for DurableCallWithRetry operations |

---

## Multi-Instance

### --peer-schemas

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Comma-separated list of peer cleat schemas |

Enables cross-instance child workflows and signals between separate worker
pools sharing the same database cluster.

---

### --shards-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to shards JSON config for multi-shard operation |

JSON format:

```json
[
  {"name": "shard-0", "conn_str": "postgres://...", "tenants": ["tenant-a"]},
  {"name": "shard-1", "conn_str": "postgres://...", "tenants": ["tenant-b"]}
]
```

---

## Utility Flags

### --generate-api-key

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Generate a new API key for the given tenant UUID and exit |

Standalone mode: creates an API key, prints it, and exits immediately without
starting the worker.

Example:

    cleat-worker --db "postgres://..." --generate-api-key "00000000-0000-0000-0000-000000000000"

---

### --namespace

| Type | Default | Description |
|------|---------|-------------|
| string | `"default"` | Workflow namespace to claim from |
