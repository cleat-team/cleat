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

### --max-workflow-duration

| Type | Default | Description |
|------|---------|-------------|
| duration | `0` | Max wall-clock duration per workflow execution |

Maximum wall-clock time a single workflow execution may take (including replay).
Workflows that exceed this duration are cancelled and fail with a timeout error.
Set to `0` (default) to disable the limit.

Example: `--max-workflow-duration 5m`

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
| bool | `false` | Require signal authorization for cross-workflow signals |

> **Usable now, but still off by default.** `allowed_signals` has a writer as of
> 2026-09-02 — `PUT /api/workflows/{id}/allowed-signals`, below — so the
> instructions in this section are followable. This block used to say the flag
> was unusable, which was true from 2026-08-05 until that writer landed.
>
> The default stays `false` for a reason that is not about the writer: nothing
> populates `allowed_signals` when a workflow *starts*. Turning the flag on today
> denies every signal to every workflow until an operator makes a second API call
> for each one, so it is a per-deployment decision rather than a safe default. It
> becomes one once a workflow can declare its callers at start time.
> See IMPROVEMENT-PLAN §3.15.

When enabled, a workflow or external caller can only signal a target
workflow if its identity appears in the target's `allowed_signals` list.
Applies to WASM `cleat_signal_workflow`, `SendSignalAndWait`, plugin
`env.SignalWorkflow`, and HTTP API signal delivery.

- Workflow identity = its `defName` (definition name).
- Plugins identify by their plugin name (e.g. `"slack-notify"`).
- External HTTP API callers have no `defName`; add `"*"` (wildcard) to
  `allowed_signals` to permit them.
- An empty `allowed_signals` means deny all (fail-secure).

Set to `true` to enable signal authorization. Every workflow starts with an
empty `allowed_signals`, so grant callers before enabling the flag, not after.

#### Reading and setting `allowed_signals`

```
GET /api/workflows/{id}/allowed-signals
    → 200 {"allowed_signals": ["billing-service"]}

PUT /api/workflows/{id}/allowed-signals
    {"allowed_signals": ["billing-service", "*"]}
    → 200 {"allowed_signals": ["billing-service", "*"]}
```

`PUT` **replaces** the whole list rather than adding to it, so revoking a caller
means sending the list without them and clearing it means sending `[]`. Both
verbs are scoped to the calling tenant: a workflow belonging to another tenant
answers `404`, the same as one that does not exist, so the endpoint cannot be
used to discover which ids are in use.

`GET` always returns an array. An unset list comes back as `[]`, never `null`.

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

> Corrected 2026-08-09: was documented as `1048576` (1 MiB). The actual
> `flag.Int` default in `cmd/cleat-worker/config.go:119` is `32768` (32 KB).

| Type | Default | Description |
|------|---------|-------------|
| int | `32768` (32 KB) | WASM output buffer size in bytes |

Controls the size of the buffer used to read output from WASM guest modules.
Larger values support workflows that produce more output data.

---

### --wasm-max-string-len

> Corrected 2026-08-09: was documented as `1048576` (1 MiB). The actual
> `flag.Int` default in `cmd/cleat-worker/config.go:120` is `65536` (64 KB).

| Type | Default | Description |
|------|---------|-------------|
| int | `65536` (64 KB) | Maximum WASM string parameter length in bytes |

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

Deletes `event_history` rows for terminal workflows. The `workflow_instances`
row itself (status, result, error, def_name) is untouched by this flag --
see `--completed-workflow-retention-days` below to also reclaim that.

---

### --completed-workflow-retention-days

| Type | Default | Description |
|------|---------|-------------|
| int | `0` (disabled) | Days to retain `workflow_instances` rows for terminal workflows (done/failed/terminated) before permanently deleting them |

Unlike `--retention-days`, this deletes the workflow's own record, not just
its step-by-step event history: after this runs, a purged workflow no longer
appears in `ListWorkflows` or the admin dashboard, and its outcome (result,
error, status) is gone. Off by default -- an operator opts in after deciding
how long their own audit/compliance requirements need a workflow's outcome
retrievable. `dead_lettered` workflows are never affected by this flag; they
have their own (separate, currently unwired) deletion path. On the Go SDK a
workflow reaches that state only by exhausting a retry policy short enough to
have run on the host (see `cleat.hostRetryBudget`); a long-backoff policy
retries via durable sleep and produces a terminal error the worker's
dead-letter predicate does not match. See
`docs/operations/workflow-retention.md` and IMPROVEMENT-PLAN.md 3.88.

Any remaining `event_history` for a purged workflow is deleted in the same
pass. See `docs/operations/workflow-retention.md` for the full design
(default rationale, FK/cascade behavior per dialect, batching, metrics).

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

Resource limits. Set to `0` for unlimited.

The first three are per workflow instance. `--max-quota-schedules` is per
tenant, because a cron schedule outlives the run that created it -- counting
schedules against that run would let a workflow create its limit, exit, and be
started again.

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

### --max-quota-schedules

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max cron schedules per tenant (0 = unlimited) |

Limits how many cron schedules a tenant may hold, counted across every workflow
in that tenant rather than per run -- see the note at the top of this section.
When exceeded, `ScheduleCron` fails with a quota error naming the current count
and the limit; the workflow itself keeps running.

Only reached through the `ScheduleCron` host call. Schedules created with
`cleat schedule create` or through the admin API are counted against the quota
but are not refused by it.

---

## Multi-Tenancy

### --claim-across-tenants

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Claim runnable work for every tenant in one query instead of only this worker's own |

A worker holds one store, scoped to one tenant, and by default its dispatch
loop claims through it. That claim only ever returns rows for that one tenant --
enforced by row-level security on PostgreSQL and SQL Server, and by an explicit
`tenant_id` predicate on MySQL -- which means a non-default tenant's workflows
never execute.

Their **schedules** are the other half, and this flag covers both. The firing
loop reads due schedules through the same widened path, then re-scopes to the
schedule's own tenant before starting the run and before advancing the schedule
-- so a non-default tenant's cron fires, and the run it starts is recorded under
that tenant.

With this set, the claim sees every tenant in a single query. Each claimed
workflow then executes against a store scoped to its **own** tenant, so the
widened view lasts exactly as long as the claim; everything downstream of it --
event history, state, child workflows, schedules -- is tenant-scoped again
immediately.

The alternative would be polling each tenant separately, one query per tenant
per tick. This is one query per tick regardless of tenant count, which is the
point.

**It requires a database-side grant, and it is off by default because of that.**
Turning it on should be a deliberate act rather than something an upgrade does
for you.

| dialect | what the deployment must do |
|---------|-----------------------------|
| PostgreSQL | Apply **both** `023_cross_tenant_claim.sql` and `024_cross_tenant_schedules.sql` as a superuser. 023 creates `cleat_dispatcher` (`NOLOGIN BYPASSRLS`) to own the claim function; 024 adds the due-schedule read to the same role. They are separate grants on purpose — with 023 alone, workflows execute but cron never fires, and the warning names the file you are missing. |
| SQL Server | Add the worker's principal to the `cleat_admin` database role -- see `012_admin_role.sql`, which documents the exact statements. The role ships with no members. One grant covers both the claim and the schedule read: `fn_tenant_filter` is bound to every table involved. |
| MySQL | **Not supported on the default topology.** `MySQLStoreFactory` gives each tenant its own physical database (`cleat_<tenant_id>`), so there is no predicate to drop -- the other tenants' rows are not filtered out, they are in another database. The worker warns once and claims its own tenant. A MySQL deployment pointed at a *single shared* database does work, since there isolation really is just a `tenant_id` predicate. |

If the flag is set but the store cannot claim across tenants -- wrong dialect,
wrong topology, or the grant was never made -- the worker logs one warning
naming the reason and keeps claiming its own tenant. It does not stop claiming,
and it does not fail to start: on a mixed fleet the flag says what the operator
wants while the store says what is actually possible, and those can disagree.

A missing grant therefore narrows a worker rather than stopping it.

**The worker says which mode it is in at startup**, before either loop ticks, so
you do not have to infer it from silence:

```
INFO  cross-tenant workflow claim is available
INFO  cross-tenant due-schedule read is available
```

or, on a deployment that applied 023 but not 024:

```
INFO  cross-tenant workflow claim is available
WARN  cross-tenant due-schedule read is NOT available; only this worker's own
      tenant's cron will fire
      reason=admin.get_due_schedules does not exist; apply
             migrations/postgres/024_cross_tenant_schedules.sql
```

It is a report, not a gate -- refusing to start would contradict the degradation
above, and would turn a revoked `GRANT` into an outage for the worker's own
tenant, which was never affected.

On PostgreSQL it also checks something no runtime error explains: whether the
function's **owner still has `BYPASSRLS`**. Losing that attribute does fail --
every call raises `cleat.tenant_id is not set` (P0001), because the policies are
fail-closed -- but that message names neither the function nor the missing
privilege, and there is no path from it to `ALTER ROLE cleat_dispatcher
BYPASSRLS`. The startup line names it.

---

## Multi-Instance

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

## Observability / OpenTelemetry

### --otel-endpoint

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | OTLP HTTP endpoint for trace export (e.g., `localhost:4318`) |

When set, the worker exports OpenTelemetry traces to the specified OTLP HTTP
endpoint. Leave empty to disable trace export (default).

### --otel-disabled

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Disable OpenTelemetry trace export |

Force-disables trace export even when `--otel-endpoint` is set. Useful for
temporarily suppressing trace export without changing the endpoint configuration.
