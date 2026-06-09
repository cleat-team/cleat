# Troubleshooting Guide

Common problems encountered when building, deploying, and running cleat workflows,
organised by symptom with diagnosis steps and fixes.

> **New to cleat?** Start with the [Quick Start Tutorial](tutorials/quick-start.md).
> **Looking for a specific error code?** Jump to [Common Error Codes](#6-common-error-codes).

---

## Table of Contents

1. [WASM Build Failures](#1-wasm-build-failures)
2. [Database Connection Issues](#2-database-connection-issues)
3. [Replay Divergence Debugging](#3-replay-divergence-debugging)
4. [Plugin Issues](#4-plugin-issues)
5. [Worker Scaling and Performance](#5-worker-scaling--performance)
6. [Common Error Codes](#6-common-error-codes)

---

## 1. WASM Build Failures

### Symptom: "go build" succeeds but "cleat build" fails

When `go build ./workflows/my-workflow` compiles fine but `cleat build ./workflows/my-workflow` produces errors, the pipeline has detected a determinism violation or unsupported construct.

**Diagnosis: How to confirm**

Run `cleat vet` on your workflow to get the specific error code:

```bash
cleat vet ./workflows/my-workflow/
```

The output includes an error code (E001--E021). Each code maps to a specific violation.
See the [complete error code reference](#61-determinism-violation-codes-e001-e021) below.

**Fix: How to resolve**

Address the specific error code:

| Code | Violation | Fix |
|------|-----------|-----|
| E001 | `go` goroutine | Use `h.ChildWorkflow()` for parallelism |
| E002 | Channel send / receive | Use `h.AwaitSignals()` or `h.PollSignal()` |
| E003 | `time.Now()` | Replace with `h.Now()` |
| E004 | `time.Sleep()` | Replace with `h.DurableSleep()` |
| E005 | `net/http` | Use `h.DurableCall()` with a service name |
| E006 | `database/sql` | Use `h.DurableCall()` with a service name |
| E007, E018 | `math/rand`, `math/rand/v2` | Replace with `h.Random()` |
| E008 | Interface dispatch | Use concrete types |
| E009 | Function value call | Call functions directly by name |
| E010 | `os` package | Pass configuration via workflow input |
| E011 | `reflect` | Use compile-time generics or type switches |
| E012 | `close()` on channel | Use signals instead of channels |
| E013 | `sync.Mutex`, `sync/atomic` | Remove; workflow code is single-threaded |
| E014 | `time.After`, `time.NewTicker` | Use `h.DurableSleep()` |
| E015 | `fmt.Print*`, `log.Print*` | Use `h.DurableLog()` |
| E016 | `os.Getenv`, `os.Exit` | Pass config via workflow input; return errors |
| E017 | `crypto/rand` | Use `h.Random()` |
| E020 | Durable calls in `init()` | Move to workflow entry point |
| E021 | Map iteration | Collect and sort keys before iterating |

See [Go Workflow Constraints](workflow-go-constraints.md) for detailed explanations
and migration examples.

---

### Symptom: Missing imports after transformation

After running `cleat build`, the transformed code references `cleat.HostCalls` but
has no import statement for the `cleat` package, or the import path is wrong.

**Diagnosis: How to confirm**

Inspect the build output (printed by `cleat build` with `--verbose`) for errors like:

```
transformer: missing cleat import in file workflows/order.go
```

Or look at the intermediate transformed files in the build directory (printed by
`cleat build --dry-run`).

**Fix: How to resolve**

Make sure your workflow entry point function accepts `cleat.HostCalls` as its
first parameter:

```go
import "github.com/cleat-team/cleat/cleat"

func PlaceOrder(h cleat.HostCalls, input string) error {
    // h is available here
}
```

The transformer auto-threads `HostCalls` through the call graph, but it needs
at least one function that explicitly receives it. If all your functions only
use `HostCalls` transitively, add `HostCalls` as a parameter to at least the
exported entry point.

See [WASM Compilation: Auto-Threading](explanation/wasm-compilation.md#auto-threading)
for details on how the transformer propagates `HostCalls`.

---

### Symptom: TinyGo build produces a broken WASM module

The workflow works when compiled with the standard Go target (`--target go`) but
fails at runtime when compiled with TinyGo (`--target tinygo`). Common symptoms:
JSON parsing produces zero values, `time.Time` serialisation is wrong, or the
module panics on startup.

**Diagnosis: How to confirm**

Check which target was used by looking at the WASM binary size:

```bash
ls -lh output.wasm
# TinyGo:  50-200 KB
# Standard Go: 4-10 MB
```

Check the `go.mod` in the build output directory for the TinyGo dependency shim
at `.deps/`.

**Fix: How to resolve**

TinyGo does not fully implement the Go standard library. The following packages
are known to cause problems:

- `encoding/json` -- limited reflection support; use `cleat-gen` code generation
  for custom `MarshalJSON`/`UnmarshalJSON` methods
- `net` / `net/http` -- use `h.DurableCall()` instead
- `regexp` -- limited; use string operations
- `time` timezone loading -- use UTC only

Switch to the standard Go target if your workflow needs these packages:

```bash
cleat build --target go ./workflows/my-workflow/
```

The standard Go target produces larger binaries but has full standard library
support, no dependency shim requirement, and no JSON bugs.

See [TinyGo Limitations](workflow-go-constraints.md#5-tinygo-limitations) and
[WASM Compilation Targets](explanation/wasm-compilation.md#compilation-targets)
for the full compatibility matrix.

---

### Symptom: Python WASM build fails

Building a Python workflow fails at the `cleat build` step with an error from the
wasmtime Python runtime bundler.

**Diagnosis: How to confirm**

Look for errors mentioning `cpython`, `python3`, `libpython`, or `runtime bundle`
in the build output.

**Fix: How to resolve**

Python WASM support requires the wasmtime backend (`--backend wasmtime`).
Ensure that:

1. The worker was compiled with CGo enabled and the wasmtime backend included
   (`backend_wasmtime.go` requires CGo).
2. The CPython WASM runtime bundle is available at the expected path (set
   `--python-runtime` or `CLEAT_PYTHON_RUNTIME`).
3. The python-sdk is installed and the workflow uses `from cleat import ...` imports.

See the [Python SDK documentation](../python-sdk/README.md) for setup instructions.

---

## 2. Database Connection Issues

### Symptom: Worker fails to start -- "no database connection string found"

```
Error: no database connection string found: set --db, DATABASE_URL, or CLEAT_DATABASE_URL
```

**Diagnosis: How to confirm**

The worker has no configured database URL. Check the credential resolution order:

```bash
# Check each source:
echo "CLI flag:  $CLEAT_DB_URL"        # --db flag
echo "Env var 1: $DATABASE_URL"         # environment variable
echo "Env var 2: $CLEAT_DATABASE_URL"   # environment variable
```

**Fix: How to resolve**

Set the database connection string via one of:

```bash
# CLI flag (highest priority):
cleat-worker --db "postgres://user:pass@host:5432/cleat?sslmode=require"

# Environment variable:
export DATABASE_URL="postgres://user:pass@host:5432/cleat?sslmode=require"

# Or for cleat-specific override:
export CLEAT_DATABASE_URL="postgres://user:pass@host:5432/cleat?sslmode=require"
```

See [Credentials](engine/credentials.go) for the full resolution logic.

---

### Symptom: PostgreSQL -- authentication failures

```
pq: password authentication failed for user "cleat"
```

**Diagnosis: How to confirm**

Check the PostgreSQL connection string format and the `pg_hba.conf` settings
on the server.

**Fix: How to resolve**

1. Verify the connection string format: `postgres://username:password@host:5432/dbname`.
2. URL-encode special characters in the password (e.g., `%40` for `@`, `%25` for `%`).
3. Check if `pg_hba.conf` allows password-based auth for the connecting IP range.
4. Test with `psql` using the same credentials:

   ```bash
   psql "postgres://username:password@host:5432/dbname"
   ```

**SSL mode issues:**

If you see `pq: SSL is not enabled on the server` but did not explicitly request SSL:

```bash
# Disable SSL (development only):
cleat-worker --db "postgres://user:pass@host:5432/cleat?sslmode=disable"

# Require SSL (production):
cleat-worker --db "postgres://user:pass@host:5432/cleat?sslmode=require"
```

For production, use `sslmode=verify-full` with a CA certificate:

```bash
cleat-worker --db "postgres://user:pass@host:5432/cleat?sslmode=verify-full&sslrootcert=/path/to/ca.pem"
```

---

### Symptom: MySQL -- SSL/TLS errors

```
Error 2026 (HY000): TLS/SSL error
or
driver: bad connection
```

**Diagnosis: How to confirm**

MySQL 8.0+ defaults to `--require-secure-transport` which rejects non-TLS
connections from non-localhost users.

**Fix: How to resolve**

**Option 1: Configure the server to allow non-SSL connections (development only):**

```sql
ALTER USER 'cleat'@'%' REQUIRE NONE;
FLUSH PRIVILEGES;
```

**Option 2: Connect with SSL (production):**

```bash
cleat-worker --db "cleat:password@tcp(host:3306)/cleat?tls=true"
```

With a custom CA:

```bash
cleat-worker --db "cleat:password@tcp(host:3306)/cleat?tls=custom&tls-ca=/path/to/ca.pem"
```

**Timezone errors:**

MySQL's default timezone can cause timestamp mismatches. Set the timezone in the
connection string:

```bash
cleat-worker --db "cleat:password@tcp(host:3306)/cleat?parseTime=true&loc=UTC"
```

---

### Symptom: MSSQL -- driver setup or authentication failure

```
login error: mssql: cannot connect: driver name is not supported
or
login error: Login failed for user '...'
```

**Diagnosis: How to confirm**

MSSQL authentication depends on the driver configuration. The worker uses the
`mssql` driver (`github.com/denisenkom/go-mssqldb`).

**Fix: How to resolve**

**SQL Server Authentication:**

```bash
cleat-worker --db "sqlserver://sa:password@host:1433?database=cleat&encrypt=disable"
```

**Windows Authentication (integrated security):**

```bash
cleat-worker --db "sqlserver://host:1433?database=cleat&encrypt=disable&trusted_connection=yes"
```

**Common MSSQL issues:**

- `encrypt=disable` is required for development instances that do not have a
  TLS certificate configured.
- TLS certificate hostname mismatch: set `TrustServerCertificate=true` or
  `encrypt=true&TrustServerCertificate=true`.
- For SQL Server Express or named instances, use the `instance` parameter:
  `sqlserver://host:1433?instance=INSTANCENAME&database=cleat`.

Connection pools are configured per-tenant when using MSSQL RLS; see
[mssql_store.go](engine/mssql_store.go) for details.

---

### Symptom: "SKIP LOCKED" errors in logs

```
ERROR: could not obtain lock on row in relation "workflow_instances"
```

**Diagnosis: How to confirm**

This usually indicates a database backend that does not support `SKIP LOCKED`,
or a transaction isolation issue.

**Fix: How to resolve**

- **PostgreSQL**: `SKIP LOCKED` is supported in PostgreSQL 9.5+. Ensure your
  server version meets this requirement.
- **MySQL**: `SKIP LOCKED` is supported in MySQL 8.0+ and MariaDB 10.6+.
  Older versions will return a syntax error.
- **MSSQL**: `SKIP LOCKED` syntax differs slightly. The engine's `READPAX`
  hint is used instead; see [mssql_store.go](engine/mssql_store.go).

The `SKIP LOCKED` query is the core claim pattern:

```sql
SELECT * FROM workflow_instances
WHERE status = 'ready' AND next_wake_at <= now()
ORDER BY created_at ASC
LIMIT 20
FOR UPDATE SKIP LOCKED;
```

If this query produces errors, your database version or configuration does not
support this feature. Upgrade your database or check that the statement timeout
is not too low (the query waits briefly for row-level locks).

See [Execution Engine: Claim Loop](explanation/execution-engine.md#claim-loop)
for details of the claim pattern.

---

## 3. Replay Divergence Debugging

### Symptom: "replay divergence" errors in logs

```
replay divergence at step 0: expected call event, got sleep.
  actual request: { truncated }
  expected request: { truncated }
Run 'cleat vet' on your workflow code to check for common non-determinism issues
```

**Diagnosis: How to confirm**

Replay divergence occurs when the workflow produces a different sequence of
host function calls during replay than it did during original execution. Each
divergence error includes:

- The step number where divergence was detected.
- What the replaying workflow called compared to what the event history expects.
- Relevant payload data (truncated with a hash for large payloads).

**Fix: How to resolve**

1. **Run `cleat vet`** on your workflow code to catch determinism violations
   that the build pipeline may have missed:
   ```bash
   cleat vet ./workflows/my-workflow/
   ```

2. **Check for non-deterministic patterns**:
   - Map iteration without sorted keys (E021)
   - Floating-point values in control flow (W002)
   - External input sources not going through `HostCalls`

3. **Inspect the event history** for the failed workflow to compare what was
   recorded vs what was replayed:
   ```bash
   cleatctl events get --workflow-id <id>
   ```

4. **Verify that all external communication uses `h.DurableCall()`**.
   Direct HTTP, database, or file I/O during execution will not be recorded in
   event history and will diverge on replay.

See [Determinism in Cleat Workflows](determinism.md) for a complete list of
determinism rules and common pitfalls.

---

### Symptom: Checksum verification failures

```
cleat_replay_checksum_failures_total: 1
checksum mismatch at step 42 for workflow wf-abc-123
```

**Diagnosis: How to confirm**

Each event in the event history has a SHA-256 checksum. During replay, the
engine verifies that the checksums match. A mismatch means the event data was
corrupted or tampered with, or the workflow produced different output for a
replayed step.

**Fix: How to resolve**

1. **Check for data corruption**: Verify that the `event_history` table is not
   being modified externally (e.g., by direct SQL updates, backups restoring
   from a different point in time).
2. **Check for non-determinism**: If the workflow produced different output for
   the same input, check for time-based logic, random number generation, or
   external API calls that bypass `DurableCall`.
3. **Temporarily tolerate mismatches** (emergency only): Set
   `failOnChecksumMismatch` to `false` to downgrade checksum failures from
   abort to warning. This should only be used as a temporary measure while
   investigating the root cause.

See [Determinism: Event History Integrity](determinism.md#event-history-integrity).

---

### Symptom: "version mismatch" or "stale WASM version" errors

```
version validation: workflow instance wf-abc expects version 5 but deployed version is 6
```

**Diagnosis: How to confirm**

A workflow instance was created with one version of the WASM binary, but the
worker is trying to replay it with a different version.

**Fix: How to resolve**

This can happen when:

1. **A new version was deployed while instances were in-flight on the old version.**
   The old instances continue to replay against their original version because
   the WASM binary is keyed by `def_name:def_version` in the module cache.

2. **Workflow definitions were garbage collected.**
   If the version GC removed older versions, in-flight instances cannot find
   their WASM binary. Check `cleatctl versions list` and adjust the GC
   retention policy:
   ```bash
   cleat-worker --gc-min-versions 5 --gc-max-age 60d
   ```
   See [version_gc.go](engine/version_gc.go) for GC configuration.

3. **Rollback scenario.**
   If you need to replay an instance against a different version after a
   rollback, use the `WithAllowVersionMismatch` engine option as an escape
   hatch. This should only be used temporarily:
   ```go
   engine.WithAllowVersionMismatch(true)
   ```

**Emergency workaround**: If a version was deleted by GC and you need it back,
redeploy the workflow at the same version number or restore the WASM binary
from a backup.

---

### Symptom: Event history is too large, replay is slow

A long-running workflow with thousands of events takes minutes to replay.

**Diagnosis: How to confirm**

Check the event count for the workflow:

```bash
cleatctl events count --workflow-id <id>
```

If the event count exceeds 1000, compaction may help.

**Fix: How to resolve**

Compaction replaces sequential events with summary snapshots, reducing both
storage and replay time. Enable it with:

```bash
cleat-worker --compaction-threshold 1000 --compaction-interval 5m
```

- `--compaction-threshold`: Event count threshold that triggers compaction
  (default: 1000).
- `--compaction-interval`: How often the compaction loop runs (default: 5m).

For very long-running workflows, consider using `ContinueAsNew` to split the
workflow into multiple runs with fresh event histories.

See [Execution Engine: Compaction](explanation/execution-engine.md#compaction).

---

## 4. Plugin Issues

### Symptom: Plugin fails to initialise -- "plugin not found" or "Init failed"

```
Error: plugin "my-plugin" not found in registry
or
Error: plugin "my-plugin" Init failed: missing required config key "api_key"
```

**Diagnosis: How to confirm**

Plugins load when the worker starts. Check the startup logs for plugin
registration and initialisation messages. Each plugin's `Init()` method is
called in dependency order.

**Fix: How to resolve**

1. **Plugin not registered**: Verify that the plugin package is imported in the
   worker binary. Plugins register themselves via `init()` functions. If a
   plugin is not imported, it will not appear in the registry.
   ```go
   import _ "github.com/cleat-team/cleat/plugins/slacknotify"
   ```

2. **Missing or invalid config**: Check the plugin configuration file
   (`--plugin-config`). The configuration is loaded at startup and passed to
   `plugin.Environment.Config`. Required configuration keys vary by plugin;
   check the plugin's documentation.

3. **Dependency not satisfied**: Some plugins depend on other plugins or
   services. The `Requires` field in `PluginInfo` lists dependencies. The
   engine loads plugins in topological order; if a dependency is missing, the
   plugin fails to initialise.

See [Plugin Reference](plugin/plugin.go) for the `Plugin` interface and
`PluginInfo` structure.

---

### Symptom: Database migration errors during plugin startup

```
Error: plugin "my-plugin" migration failed: column "new_field" does not exist
```

**Diagnosis: How to confirm**

Plugins that need database tables run migrations during `Init()`. Migration
errors usually mean the schema is out of sync between the plugin version and
the database.

**Fix: How to resolve**

1. **Run pending migrations**: Some plugins expose a migration command or apply
   migrations on startup. Check the plugin's documentation for the correct
   migration procedure.

2. **Check the plugin `DatabaseAccess` level**: Plugins declare their database
   access requirements in `PluginInfo`:
   - `DatabaseAccessNone` -- no database access.
   - `DatabaseReadOnly` -- read-only queries.
   - `DatabaseReadWrite` -- read/write access including schema migrations.

   A plugin with `DatabaseReadWrite` may create or alter tables during `Init()`.
   If the database user lacks DDL permissions, migrations will fail.

3. **Manual schema fix**: If the migration script is failing on a specific
   statement, you can apply the change manually and restart the worker. The
   migration should detect that it has already been applied and skip it.

---

### Symptom: Plugin dependency ordering -- "circular dependency" or topological sort failure

```
Error: plugin "A" and plugin "B" have a circular dependency
```

**Diagnosis: How to confirm**

The plugin loader computes a topological order from each plugin's `Requires`
field. If plugin A requires B and B requires A, the sort fails.

**Fix: How to resolve**

1. Break the circular dependency by removing one of the `Requires` entries.
2. If both plugins truly depend on each other, consider merging them into a
   single plugin or extracting the shared dependency into a third plugin that
   both depend on.

---

### Symptom: Plugin panic -- worker logs a panic and the plugin is marked unhealthy

```
[plugin] my-plugin panicked: runtime error: index out of range [5] with length 3
...
plugin "my-plugin" is unhealthy: plugin "my-plugin" panicked: ...
```

**Diagnosis: How to confirm**

When a Go-compiled plugin panics during a host function call, the `RecoverPluginFunc`
wrapper catches the panic and marks the plugin as unhealthy. Subsequent calls to
any host function in that plugin return a `PanicError` without executing the function.

Check the plugin health status:

```bash
cleatctl plugin status
```

**Fix: How to resolve**

1. **Identify the root cause**: The panic error includes the panic value and
   the full goroutine stack trace. Look at the log lines from the panic event.
2. **Restart the worker**: The `PluginHealthTracker` is in-memory only. Restarting
   the worker clears the unhealthy state and re-initialises all plugins.
3. **Fix the bug**: The stack trace pinpoints the exact line in the plugin code
   that panicked. Fix the bug and redeploy the worker.

**Long-term solution**: Compile plugins as WASM modules instead of linking them
into the worker binary. WASM provides process-level isolation, so a plugin crash
cannot affect the worker or other plugins. See the [plugin recovery docs](plugin/recovery.go)
for details on the crash recovery boundary.

---

## 5. Worker Scaling and Performance

### Symptom: Claim contention -- multiple workers fighting for the same instances

```
worker A claimed instance wf-123
worker B attempted to claim instance wf-123 but it was already locked
```

**Diagnosis: How to confirm**

The claim loop uses `SELECT ... FOR UPDATE SKIP LOCKED` to prevent duplicate
claims. If workers are fighting over instances, check the sticky worker fast path.

**Fix: How to resolve**

1. **Verify sticky worker assignment**: After claiming an instance, the worker
   records itself as the sticky worker. Subsequent polls use the sticky fast path
   to reclaim instances previously assigned to this worker. Check that sticky
   assignment is working correctly:
   ```sql
   SELECT id, assigned_to, sticky_worker_id FROM workflow_instances WHERE status = 'running';
   ```

2. **Reduce the number of workers**: If too many workers are polling the same
   queue, reduce the worker count or increase the poll interval.

3. **Check the reaper interval**: The reaper reclaims instances with stale
   heartbeats every 30 seconds. If instances are being claimed and then quickly
   released, the heartbeat interval (default 5s) may be too long for your
   workload:
   ```bash
   cleat-worker --heartbeat 2s
   ```

See [Execution Engine: Claim Loop](explanation/execution-engine.md#claim-loop)
and [Execution Engine: Sticky Workflow Fast Path](explanation/execution-engine.md#sticky-workflow-fast-path).

---

### Symptom: Heartbeat failures -- worker marked as dead

```
reaper: reclaimed instances from worker dead-worker-123 (stale heartbeat)
```

**Diagnosis: How to confirm**

The reaper reclaims instances where `heartbeat_at` is older than 30 seconds
(6x the default 5s heartbeat interval). This indicates the worker stopped
sending heartbeats.

**Fix: How to resolve**

1. **Check worker process health**: Is the worker process still running?
   Check for OOM kills, segfaults, or other process termination.

2. **Check database connectivity**: A temporary database connection loss will
   cause heartbeat failures. The worker enters a reconnect loop with exponential
   backoff (1s, 2s, 4s, ... up to 30s). Check the worker logs for connection
   errors.

3. **Adjust the heartbeat interval**: If your workload has many in-flight
   workflows that each generate heartbeat queries, the cumulative database load
   may cause delays:
   ```bash
   cleat-worker --heartbeat 10s
   ```

4. **Verify instance ownership**: The heartbeat `UPDATE` checks
   `assigned_to = <worker_id>`. If another worker stole the instance (e.g., after
   a network partition), the heartbeat silently fails, and the original worker
   releases the instance. This is normal behaviour -- the instance will proceed
   on the new worker.

See [Execution Engine: Heartbeat / Keepalive](explanation/execution-engine.md#heartbeat--keepalive)
and [Execution Engine: Reaper](explanation/execution-engine.md#reaper).

---

### Symptom: Memory pressure -- WASM module cache growing too large

```
WASM cache: evicting entry my-workflow:v5 (total size: 2.1 GB, limit: 1 GB)
```

**Diagnosis: How to confirm**

The WASM module cache is an LRU cache keyed by `def_name:def_version`. Standard
Go WASM binaries are 4-10 MB each. With many workflow definitions and versions,
the cache can consume significant memory.

Monitor cache metrics:

- `cleat_wasm_cache_entries` -- current number of cached modules.
- `cleat_wasm_cache_bytes` -- total cached bytes.
- `cleat_wasm_cache_evictions` -- eviction rate.

**Fix: How to resolve**

1. **Reduce WASM binary size**: Use `--target tinygo` for workflows that do not
   need the full Go standard library. TinyGo binaries are ~98% smaller.
   See [Binary Size Comparison](workflow-go-constraints.md#binary-size-comparison).

2. **Adjust cache limits**: Configure the maximum entries and total bytes:
   ```bash
   cleat-worker --wasm-cache-size 500 --wasm-cache-bytes 2GB
   ```

3. **GC old versions**: Remove deprecated workflow versions that are no longer
   needed:
   ```bash
   cleatctl versions gc --dry-run
   cleatctl versions gc  # remove --dry-run to apply
   ```
   See [version_gc.go](engine/version_gc.go) for configuration.

4. **Tune the LRU eviction policy**: The cache evicts the least recently used
   entry when the max entries or max bytes limit is hit. High eviction rates
   indicate the cache is too small for your deployment.

---

### Symptom: Slow workflow execution -- performance issues

A workflow that should complete in seconds is taking minutes.

**Diagnosis: How to confirm**

1. **Enable tracing**: If the worker is configured with an OTLP endpoint, each
   workflow execution creates a root span with child spans per event step.
   Check the tracing dashboard for slow steps.

2. **Check WASM cold start time**: Loading a WASM module from the database
   takes 50-100 ms for standard Go modules, 0.5-2 ms for TinyGo. If you see
   long load times, check the `wasm_bytes` column size in `workflow_defs`.

3. **Profile with `cleat build --bench`**:
   ```bash
   cleat build --bench ./workflows/my-workflow/
   ```

**Fix: How to resolve**

1. **Use TinyGo for smaller binaries** and faster cold starts:
   ```bash
   cleat build --target tinygo ./workflows/my-workflow/
   ```

2. **Reduce WASM module size**: Avoid importing large packages like
   `encoding/json` (adds 1-2 MB) in workflow code. Use `cleat-gen` code
   generation for JSON serialisation.

3. **Check the event count**: If the workflow has many events, compaction
   may help (see [Event history replay is slow](#symptom-event-history-is-too-large-replay-is-slow)).

4. **Check database query performance**: Slow `SELECT ... FOR UPDATE SKIP LOCKED`
   queries can delay the claim loop. Check for missing indexes on
   `workflow_instances(tenant_id, status, next_wake_at)`.

5. **Check the circuit breaker**: If the worker encountered consecutive database
   errors, it backs off with exponential backoff (up to 30s). Check for
   `circuit breaker open` or `backoff` in the worker logs.

See [Execution Engine: WASM Module Cache](explanation/execution-engine.md#wasm-module-cache)
and [Execution Engine: Tracing](explanation/execution-engine.md#tracing).

---

## 6. Common Error Codes

### 6.1 Determinism Violation Codes (E001--E021)

These codes are emitted by the `cleat build` pipeline's static analyser
(in [internal/closure/closure.go](../internal/closure/closure.go)).

| Code | Severity | Rule | Suggestion |
|------|----------|------|------------|
| E001 | Error | Goroutines (`go f()`) | Use `h.ChildWorkflow()` |
| E002 | Error | Channel send / receive | Use `h.AwaitSignals()` |
| E003 | Error | `time.Now()` | Use `h.Now()` |
| E004 | Error | `time.Sleep()` | Use `h.DurableSleep()` |
| E005 | Error | `net/http` import | Use `h.DurableCall()` |
| E006 | Error | `database/sql` import | Use `h.DurableCall()` |
| E007 | Error | `math/rand` import | Use `h.Random()` |
| E008 | Error | Interface dispatch | Use concrete types |
| E009 | Error | Function value calls | Direct function call |
| E010 | Error | `os` package reference | Pass via workflow input |
| E011 | Error | `reflect` package reference | Generics or type switches |
| E012 | Error | `close()` on channel | Signals instead |
| E013 | Error | `sync.Mutex`, `sync/atomic` | Remove (single-threaded) |
| E014 | Error | `time.After`/`NewTicker`/`NewTimer` | `h.DurableSleep()` |
| E015 | Error | `fmt.Print*` / `log` output | `h.DurableLog()` |
| E016 | Error | `os.Getenv` / `os.Exit` | Workflow input / return error |
| E017 | Error | `crypto/rand` import | `h.Random()` |
| E018 | Error | `math/rand/v2` import | `h.Random()` |
| E020 | Error | Durable calls in `init()` | Move to entry point |
| E021 | Error | Non-deterministic map iteration | Sort keys before iterating |
| W001 | Warning | Map iteration in non-critical path | Use sorted keys |
| W002 | Warning | Float in control flow | Use `math.Float64bits()` |

See the full [Go Workflow Constraints](workflow-go-constraints.md#4-complete-error-code-reference)
reference for detailed explanations of each code.

---

### 6.2 Engine Error Codes

These codes classify runtime errors for retry decisions. They appear in the
`error_code` column of `workflow_instances` and in structured log output.

| Code | String representation | Meaning |
|------|----------------------|---------|
| `ErrUnknown` | `"unknown"` | Unclassified error |
| `ErrTransient` | `"transient"` | Retryable (DB connection lost, timeout) |
| `ErrPermanent` | `"permanent"` | Non-retryable (invalid input, not found) |
| `ErrCancelled` | `"cancelled"` | Workflow was cancelled |
| `ErrTimeout` | `"timeout"` | Execution exceeded its deadline |
| `ErrAmbiguous` | `"ambiguous"` | Call outcome unknown after crash; caller should check the external service before retrying |
| `ErrRetriesExhausted` | `"retries_exhausted"` | All retry attempts were exhausted |

**Retry behaviour:**

- Only `ErrTransient` is automatically retried by the engine.
- `ErrAmbiguous` is NOT retried automatically because the call may have succeeded
  on the server side even though the response was not persisted.
- `ErrPermanent` is never retried.

See [engine/errors.go](engine/errors.go) for the implementation.

---

### 6.3 Plugin Error Codes

| Error | Source | Meaning |
|-------|--------|---------|
| `PanicError` | `plugin/recovery.go` | Plugin panicked during host function execution. Plugin is marked unhealthy. |
| `"plugin not found"` | Plugin registry | The requested plugin name is not registered. Check imports. |
| `"Init failed"` | `plugin.Plugin.Init()` | Plugin initialisation returned an error. Check config and dependencies. |
| `"circular dependency"` | Plugin loader | Two or more plugins have circular `Requires` references. |
| `"migration failed"` | Plugin `Init()` | Database migration during plugin init failed. Check schema version. |

On panic, the `PluginHealthTracker` marks the plugin unhealthy and subsequent
invocations return the panic error without calling into the plugin. Restart
the worker to recover.

See [plugin/recovery.go](plugin/recovery.go) for panic recovery wrappers and
the `PanicError` type.

---

## Cross-Reference Index

| Issue | Related Documentation |
|-------|---------------------|
| Build-time determinism errors | [Go Workflow Constraints](workflow-go-constraints.md) |
| WASM transformer pipeline | [WASM Compilation](explanation/wasm-compilation.md) |
| Determinism rules | [Determinism Guide](determinism.md) |
| Replay mechanism | [Execution Engine](explanation/execution-engine.md) |
| Plugin interface and lifecycle | [Plugin Reference](plugin/plugin.go) |
| WASM sandbox and security | [Security Model](explanation/security-model.md) |
| Worker configuration flags | [CLI Reference](reference/cli.md) |
| Database schema | [Database Reference](reference/database.md) |
| Testing workflows with replay | [Testing Workflows](how-to/test-workflows.md) |
