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

### --driver

| Type | Default | Description |
|------|---------|-------------|
| string | `"postgres"` | Database driver: `postgres`, `mysql`, or `mssql` |

Selects the database backend. Must match the `--db` URL scheme.

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

### --health-check-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `30s` | Interval for background loop health checks |

The worker's internal watchdog checks the health of background loops (polling,
compaction, memory monitoring) at this interval. Set to `0` to disable the
watchdog entirely.

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

### --require-signal-auth

| Type | Default | Description |
|------|---------|-------------|
| bool | `true` | Require signal authorization for cross-workflow signals |

When enabled, a workflow or external caller can only signal a target
workflow if its identity appears in the target's `allowed_signals` list.
Applies to WASM `cleat_signal_workflow`, `SendSignalAndWait`, plugin
`env.SignalWorkflow`, and HTTP API signal delivery.

- Workflow identity = its `defName` (definition name).
- Plugins identify by their plugin name (e.g. `"slack-notify"`).
- External HTTP API callers have no `defName`; add `"*"` (wildcard) to
  `allowed_signals` to permit them.
- An empty `allowed_signals` means deny all (fail-secure).

Set to `false` to disable signal authorization (backward compatible).

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

## DB Credentials

### --db-credential-provider

| Type | Default | Description |
|------|---------|-------------|
| string | `"env"` | DB credential provider: `env`, `vault`, or `aws-secrets-manager` |

Selects how the worker obtains database credentials. The `env` provider reads
the `--db` connection URL directly. The `vault` and `aws-secrets-manager`
providers fetch credentials from HashiCorp Vault or AWS Secrets Manager
respectively, using the path specified by `--db-credential-path`.

---

### --db-credential-path

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path/name for credential provider |

For the `vault` provider, this is the Vault secret path (e.g.,
`secret/data/cleat/db`). For `aws-secrets-manager`, this is the AWS secret
name or ARN. Ignored when `--db-credential-provider` is `env`.

---

## Encryption at Rest

### --encryption-key-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to file containing base64-encoded AES-256-GCM encryption key |

The file must contain a base64-encoded 32-byte (256-bit) key. When set together
with `--encrypt-sensitive-payloads`, sensitive payload fields in event history
are encrypted using AES-256-GCM before being written to the database.

---

### --encrypt-sensitive-payloads

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Enable encryption of sensitive event payload fields |

When enabled, the worker encrypts sensitive fields (e.g., signal payloads,
activity results) using the key from `--encryption-key-file`. Requires
`--encryption-key-file` to also be set.

---

## Plugins

### --plugin-config

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to a plugin configuration JSON file |

The file contents are expanded with `os.ExpandEnv` before parsing.

---

### --max-plugin-connections

| Type | Default | Description |
|------|---------|-------------|
| int | `10` | Maximum database connections across all plugins |

Plugins that need database access share a connection pool capped at this limit.
Set to `0` to let each plugin use the main worker connection pool directly.

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

### --rate-limit-per-tenant

| Type | Default | Description |
|------|---------|-------------|
| float | `0` | Requests/second per tenant (0 = disabled) |

Per-tenant rate limiting, independent of the per-IP rate limit. Requires
`--require-auth` to be enabled so the worker can identify the tenant from
the API key or header.

---

### --rate-limit-per-tenant-burst

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Burst size for per-tenant rate limit |

Maximum burst size allowed above the per-tenant rate limit. Only meaningful
when `--rate-limit-per-tenant` is set to a non-zero value.

---

## WASM

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

### --wasm-memory-max-mb

| Type | Default | Description |
|------|---------|-------------|
| int | `32` | Max WASM linear memory per module in MB |

Corresponds to 512 WASM pages at 32 MB (64 KiB per page). Set to `0` to use
the wazero default. Increasing this allows workflows with larger memory
requirements to execute.

---

### --wasm-instruction-limit

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max WASM instructions per invocation |

Limits the number of WASM instructions a single workflow invocation can
execute. Set to `0` for no limit. Enforced via a wazero function listener.

---

### --wasm-output-buffer-size

| Type | Default | Description |
|------|---------|-------------|
| int | `1048576` (1 MiB) | WASM output buffer size in bytes |

Controls the size of the buffer used to read output from WASM guest modules.
Larger values support workflows that produce more output data.

---

### --wasm-max-string-len

| Type | Default | Description |
|------|---------|-------------|
| int | `1048576` (1 MiB) | Maximum WASM string parameter length in bytes |

Limits the length of string parameters passed to WASM guest functions.
Strings longer than this are truncated before reaching the guest.

---

### --wasm-cache-dir

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Directory for disk-backed compiled WASM module cache |

When set, compiled WASM modules are cached to disk, reducing startup latency
on worker restarts. Leave empty to disable disk caching (in-memory only).

---

### --wasm-disk-cache-max-files

| Type | Default | Description |
|------|---------|-------------|
| int | `100` | Max files in the disk-backed compiled WASM module cache |

Controls the maximum number of compiled module files stored on disk. Uses LRU
eviction when the limit is reached. Only meaningful when `--wasm-cache-dir`
is set.

---

## Data Management

### --retention-days

| Type | Default | Description |
|------|---------|-------------|
| int | `30` | Days to retain completed/failed workflow event history (0 disables) |

---

### --redact-patterns-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to file with custom redaction patterns (one per line) |

When set, the worker reads regex patterns from the specified file and applies
them to redact sensitive fields in event history and logs. Each line in the
file is a separate pattern. Leave empty to use only built-in defaults.

---

### --max-retries

| Type | Default | Description |
|------|---------|-------------|
| int | `100` | Maximum retry attempts for DurableCallWithRetry operations |

---

### --disable-checksum-verification

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Disable event history checksum verification on replay |

By default, the worker verifies event history checksums during replay to
detect corruption. Set to `true` to skip verification (e.g., for performance
during bulk replay operations).

---

## Resource Quotas

Per-workflow-instance resource limits. Set to `0` for unlimited.

### --max-quota-events

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max events per workflow (0 = unlimited) |

Limits the total number of events a single workflow instance can generate.
When exceeded, the workflow is terminated with a quota error.

---

### --max-quota-children

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max child workflows per workflow (0 = unlimited) |

Limits the number of child workflows a single parent workflow can spawn.
When exceeded, further child start attempts fail with a quota error.

---

### --max-quota-concurrency-keys

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max concurrency keys per workflow (0 = unlimited) |

Limits the number of distinct concurrency keys a single workflow can register.
When exceeded, further key registrations fail with a quota error.

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
