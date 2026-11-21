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
| string | `""` (required) | `CLEAT_DATABASE_URL` |

PostgreSQL (or MySQL/SQL Server) connection URL. The worker connects to your
existing database; it does not manage it. Example:

    --db "postgres://user:pass@localhost:5432/cleat?sslmode=disable"
    --db "user:pass@tcp(localhost:3306)/cleat"
    --db "sqlserver://user:pass@localhost:1433?database=cleat"

The MySQL form carries no `mysql://` scheme prefix, and that is deliberate: `go-sql-driver/mysql`
has no concept of a scheme and does not strip one, so a `mysql://` DSN is unusable — in two
different ways, depending on its shape:

- `mysql://user:pass@host:3306/db` is rejected by the driver's own parser, which reads the address
  as a network name and reports `default addr for network 'host:3306' unknown`. It never reaches a
  server, so the message names neither the DSN nor the scheme.
- `mysql://user:pass@tcp(host:3306)/db` parses, with `mysql` read as the username, so the connection
  is attempted as that literal user and refused as one (`Access denied for user 'mysql'@…`).

Neither message names the scheme, which is why the tools refuse such a DSN rather than letting it
through. `cleatctl` rejects a `mysql://`-prefixed DSN before any driver sees it, naming the scheme
and giving the correct form (cleat#2962); `cleat`'s PostgreSQL-only subcommands refuse a MySQL-shaped
DSN too, naming the dialect. Dialect detection still *recognises* a `mysql://` prefix — as a
heuristic for what someone may paste, not as a claim that the DSN connects — so a recognised shape
is not a working one here (cleat#2938).

A MySQL session whose `time_zone` is not UTC introduces a DST-sized blind spot in
`--idempotency-key-retention`'s bound — see `docs/durable-calls.md`'s "Compared against the
store's own clock" section for the exact `time_zone` DSN parameter to pin it, if that precision
matters to your deployment.

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
`CREATE SCHEMA IF NOT EXISTS` on startup.

**Core tables honour this as of [#1287](https://github.com/cleat-team/cleat/issues/1287); two things
still do not.** Before that fix the flag set where the runtime connection *looked* and not where the
migrations *built*, so a non-default value did not degrade — the worker could not finish its core
migrations at all. What it now covers, and what it does not:

| | follows `--schema` |
|---|---|
| core tables, indexes, procedures, `schema_migrations` | yes |
| plugin tables and `plugin_migrations` | yes |
| the `admin` and `cleat` schemas | **no** — those names are fixed, so two pools in one database share them |

So a worker's own tables are isolated. "Multiple isolated worker pools on a single database cluster"
is a stronger claim than that and is **not** yet supported: `admin` and `cleat` are shared, and the
`SECURITY DEFINER` functions in `admin` have not been examined for what they do when two pools use
them. Treat `--schema` as "put this worker's tables somewhere other than public", not as a tenancy
boundary.

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

### --cors-allowed-origins

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Comma-separated list of exact origins (e.g. `https://app.example.com`) allowed to make cross-origin requests to this worker's HTTP API |

**Off by default.** An empty value disables CORS entirely: no preflight
handling, no `Access-Control-*` response headers, byte-identical behavior to
a worker built without this flag. There is no implicit default-allow
fallback.

When set, each listed origin gets exact-match CORS support: preflight
(`OPTIONS`) requests get the allowed methods and headers back, and ordinary
requests get `Access-Control-Allow-Origin` set to the matched origin.
Origins are matched by **exact string equality** — there is no wildcard
matching, and `*` is never written to a response header, so the unsafe
combination of a wildcard origin with a credentialed request has no code
path that can produce it. A disallowed origin's preflight is refused (403);
a disallowed origin's ordinary request reaches the worker normally (so a
non-browser caller with an unrecognised `Origin` header is never
server-side blocked) but gets no CORS headers, which is what makes the
browser itself withhold the response from that origin's own JavaScript.

**A browser holding a tenant API key is a deployment choice with real
risk.** This worker's auth is a bearer token in the `Authorization`
header — exactly the kind of credential a CORS allowlist exists to let a
browser attach to a cross-origin request. If that token lives in
JavaScript running in a browser, any script able to run on an allowed
origin (a dependency, an XSS, a compromised build step) can read and
exfiltrate it. **The recommended default is the same-origin proxy pattern
(see `#2344`/the fullstack template)**, where the frontend never holds a
cleat API key at all and the proxy injects it server-side. Reach for this
flag only when a same-origin proxy genuinely isn't an option for your
deployment, and treat the origins you list as holders of a real credential.

---

### --health-check-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `30s` | Interval for background loop health checks |

The worker's internal watchdog checks the health of background loops (polling,
compaction, memory monitoring) at this interval. Set to `0` to disable the
watchdog entirely.

---

## Schema migration

A worker does **not** migrate the database when it starts (cleat#2117). A normal
start verifies the schema and refuses to start if it is behind; migrating is a
deploy step.

### --migrate-only

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Apply the core and plugin migrations and exit |

Exits `0` when the schema is current and non-zero on any failure, having started
nothing else: it exits before the worker registers and before it reads the secrets,
so it needs no master key. Use `--migrate-db` for a DSN with DDL rights (default:
`--db`). Idempotent: a second run on a migrated database changes nothing. Safe if
several run at once, or alongside a `--migrate-on-start` worker: they queue on a
named lock in the database, on every dialect. A plugin migration that fails fails
the run.

---

### --migrate-on-start

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Let this worker apply pending migrations at startup |

The behaviour every worker had before cleat#2117, kept as an explicit opt-in for a
single node or development, where there is no deploy step. A fleet should use
`--migrate-only`.

**What a normal start does instead.** It reads the migration tracking tables (as the
runtime role, no DDL rights needed) and:

| the schema is | the worker |
|---|---|
| **behind** — a migration this binary ships is not applied | refuses to start; the message says which and how to fix it |
| **equal** | starts |
| **ahead** — migrations applied that this binary does not ship | starts, and logs a warning naming both versions |

Ahead starts because a rolling upgrade migrates to the new version while workers on
the old one are still running or restarting; refusing there would wedge the rollout
on the workers it is replacing. It relies on migrations staying additive within a
release line. That is the schema's rule only: it does not make a rolling upgrade
supported, and for 0.5.0 to 0.6.0 it is not (see
[Upgrading](../operations/upgrading.md#automatic-migration-recommended)).

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

### --max-reclaim-per-tick

| Type | Default | Description |
|------|---------|-------------|
| int | `200` | Maximum stale instances the reaper reclaims per tick (`0` = unbounded) |

The reaper reclaims any instance whose heartbeat predates the **reclaim
window** — derived from `--heartbeat` as `--heartbeat + a 30s tolerated-stall
margin + 2 × the database-call deadline + a fixed SQL Server reconnect
allowance + the retry interval + 1s`, about **47s** at the default 5s
heartbeat (raised from ~14.5s, cleat#3258, to actually cover a real
multi-second database-network stall), or exactly `--reclaim-timeout` when that
is set. (This is *not* the membership lease, `max(2 × --heartbeat, 10s)`, which
answers a different question and is swept separately.) **Any stall that outlasts
that window ages every running instance past it at once**, because they all
heartbeat through the same table — a migration holding `ACCESS EXCLUSIVE` at
worker boot, a database failover, a paused volume. Without a bound, the sweep
after the stall reclaims the entire running set in one statement and every
in-flight workflow replays simultaneously, against a database that has just
finished whatever stalled it.

No data is lost — fencing guarantees that — but it is a self-inflicted
thundering herd at the moment the database can least absorb one. See
[cleat#1320](https://github.com/cleat-team/cleat/issues/1320).

The bound changes the **rate** of recovery, never whether it happens: the sweep
is ordered by heartbeat age, so the longest-stale are reclaimed first and the
rest follow on later ticks. At the default `--concurrency` of 10, `200` is
twenty workers' worth per tick and the reaper ticks at most every 10s, so an
ordinary failure — one worker, or several — is reclaimed in a single tick and
never reaches this limit.

When it does bind, the worker logs at WARN:

    Reaper: hit the per-tick reclaim limit; more instances remain stale
      count=200 limit=200

That line is the only place a whole-set stall is visible: a bounded sweep and a
sweep that happened to find exactly that many look identical in the count alone.

`0` disables the bound and restores the previous behaviour.

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

The worker updates its heartbeat in the database at this interval. A run whose
heartbeat predates the **reclaim window** (see `--reclaim-timeout`, and note it
is not the lease below) is treated as abandoned and made available to other
workers.

**Must be below 150s.** `cleat-worker` refuses to start otherwise. A secret
writer (`set-secret`, `reseal-secrets`) counts a worker as live for five minutes
after its last heartbeat, and the worker re-checks its secrets after a gap longer
than `max(2 × --heartbeat, 10s)`. At 150s or more that threshold reaches the
writer's five minutes, so a stalled worker could be written past and resume
without noticing.

---

### --reclaim-timeout

| Type | Default | Description |
|------|---------|-------------|
| duration | `0` | How long a run may go without a heartbeat before another worker may claim it (`0` = derive from `--heartbeat`) |

`0` derives it as the window `--max-reclaim-per-tick` documents (about 47s at
the default heartbeat), which is the historical behaviour and changes nothing.
Set it to decouple the two: the heartbeat is how often a **live** worker checks
in, while this is how long a **dead** one's work stays stranded — and a database
outage stops the heartbeat without the worker being dead. Sizing it to a failover
window (tens of seconds for streaming replication, longer for managed Multi-AZ)
buys outage tolerance at the cost of that much extra delay before a crashed
worker's runs are recovered.

Refused if below `2 × --heartbeat`, which would reclaim runs whose workers are
heartbeating normally. It does **not** move the worker-membership lease, which
answers a different question — which workers exist, for shard distribution — and
still follows `--heartbeat`.

---

### --heartbeat-max-connections

| Type | Default | Description |
|------|---------|-------------|
| int | `3` | Database connections reserved for heartbeat writes |

Opens a small connection pool used only for heartbeat writes, isolated from the
execution pool. cleat#2009: a saturated execution pool (long-held connections
claiming or deferring workflows) can starve a heartbeat on the shared pool, and
a missed heartbeat is what triggers reclaim -- so pool exhaustion under load
looked like a dead worker. Reserving a few connections up front removes the
contention.

Set to `0` to disable the reserved pool and share the execution pool as before.
Negative values are refused at startup.

Counted in the worker's connection budget census (`--connection-budget`) as its
own `heartbeat=N` term.

**Not yet supported on a sharded deployment (`--shards-file`).** A sharded
worker's heartbeat stays on the execution pool regardless of this flag.

**Which runs it covers.** Every tenant's, on PostgreSQL and SQL Server. A worker
that serves other tenants (`--claim-across-tenants`) beats their runs through this
same pool, re-scoped to each tenant per call, so it costs no connection beyond
this flag's ceiling. Before cleat#3571 only the worker's own tenant's runs used
it, and another tenant's beat went through the execution pool. MySQL, whose tenant
is the database a pool was opened on, is single-tenant and has nothing to cover.
It protects against a saturated execution pool; it does nothing for a paused
process or a degraded network path.

A heartbeat that succeeds through this pool still records the worker's database
contact as OK (`recordDBContactOK`), the same signal an idle worker's DB ping
records -- so the DB-health gate cleat#2166 added does not distinguish which
pool answered, only whether the database is reachable.

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

## Residency

A durable sleep that is due soon can be **held on the worker** instead of suspended: the run keeps its instance and wakes in place when the timer fires, with no claim, no instantiation and no replay (cleat#4162, design: `docs/design/resident-suspended-runs.md`). It is **off unless `--resident-max-runs` is above zero**, and a refusal is always the safe answer: the wait suspends exactly as it always has, so a wrong number here can only hold less than it could.

**What is and is not measured, so you can judge the defaults.** The memory of a held instance is measured for *minimal* guests (Go 2.5 MiB, 19.6 MiB after a guest had made garbage; Python 9 to 16 MiB; C# 49 to 66 MiB), not for real workflows. Each held run is charged the larger of the measured figures, rounded up (Go 24 MiB, Python 17 MiB, C# 66 MiB; the Python and C# figures were measured on an arm64 Linux VM only, **x86-64 is not measured**). **The compiled component is deliberately not charged**: it lives in the worker's code cache (an LRU) whether or not anything is held, so a worker that ran the workflow already carries it, and the measured per-process figure (Python about 500 MiB, C# about 81 MiB) also included the Go and wasmtime baseline; it belongs to `--memory-soft-limit`, not to this budget. **A hold never outlives its code's cache entry**: a held instance keeps its compiled code alive even after the cache evicts it, so when the cache evicts a module or component every held run of it is sent back (it suspends, and the next claim replays it, which recompiles); the worst case stays bounded by the cache, as it is without residency. **None of this includes what a guest allocates before it blocks**: measured, a Python or C# guest that touched 64 MiB cost about 64 MiB more, so these are the cost of the runtime and a floor for a guest that does real work; the default budget (`--resident-max-runs` times the dearest language) can still be exceeded by real guests, and `--memory-soft-limit` is the second guard. A component the build did not stamp as C# is detected as Python and charged as Python. The fixed cost of replaying a run is measured (0.44 to 0.67 s per claim for a minimal guest); the cost per recorded event is **not**. The multiplier, the floor and the cap are the owner's starting point and **not measured**; tuning them on real workflows is cleat#4187. So the defaults are conservative, and with the per-event cost left at 0 every run gets the floor as its window.

**Go, Python and C# guests are held, and only durable sleeps so far.** Java, Rust and JavaScript have no measured per-instance figure and are refused, and the other waits (signals, promises, children, updates) have no caller on the park helper yet. A held run is in flight, so a worker that starts to drain sends every held run back first, and at the memory hard limit it sends them back too; nothing is held while the worker is over `--memory-soft-limit`.

### --resident-max-runs

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Most runs held in memory through a wait that is due soon; 0 turns residency off |

**EXPERIMENTAL, and not for production use.** It stays off by default while the rest of the park work lands (cleat#4162). **If a worker dies while it holds runs, their wakes are delayed until the runs are reclaimed.** The kill-while-held scenario (`tests/chaos`, `TestChaosSigkillWhileAHeldRunSleepsThenReclaim`) kills a worker mid-hold and shows the run is reclaimed and every effect happens exactly once, and measures the cost: the second effect came **40 to 41 s** after the first, against **9.2 to 9.5 s** with residency off, because a held run is still assigned to the dead worker, so only the reaper can take it, after its start-up grace, while a suspended run is simply claimable at its wake time. The harness ages the dead worker's lease instead of waiting out the real reclaim window, so **the production window comes on top of those figures, and is not measured**. A faster path for held runs is cleat#4275 (0.7.0). A worker that is draining or cordoned, or at its memory hard limit, already sends every held run back (tested).

A held run keeps its instance and one OS thread (measured: about one thread per held instance, 1053 for 1024 Go instances), so keep this small. The thread cost at larger counts was not measured.

**A held run's limits are paused for the hold and start again when it ends.** The wall-clock limits (`--wasm-wall-clock-ceiling` and `--max-workflow-duration`) are per execution *segment*, and a workflow that suspends and resumes gets a fresh deadline each time. A held wait gets the same: the limits do not run while the run is held, and **they restart at their full duration when it wakes**, as does the execution fence (`--wasm-instance-timeout`, which counts only the guest's own execution). So the held time is charged to none of them, and the work before the wait and the work after it do **not** add up against one limit, which is what suspending and resuming would have given. A limit still binds each segment: work after the wake that is longer than a fresh limit times out, as it would in the next segment of a suspended run.

Measured on real wasmtime (a Go guest through `Engine.Execute`, `TestAHeldRunMeetsItsOwnLimitsAndCancelOnRealWasmtime` and `TestAHeldWaitRestartsTheExecutionsLimitsNotJustExcludesTheHeldTimeOnRealWasmtime`):

| case | residency off | residency on |
|---|---|---|
| 150 ms ceiling, 300 ms wait | suspends in 1 ms | held, completes in about 302 ms |
| work 300 ms, wait 300 ms, work 300 ms under an 800 ms ceiling | suspends at the wait in about 304 ms | **completes in about 904 ms** |
| work 500 ms, wait, work 500 ms under an 800 ms ceiling | suspends at the wait in about 503 ms | completes in about 1.3 s: a restart, not just an exclusion of the held time |
| work 50 ms, wait, work 1000 ms under an 800 ms ceiling | suspends at the wait in about 54 ms | times out in the fresh segment: the limit still binds |
| the run's context cancelled while held | | the run is back as a suspension about 2 ms after the cancel |

**Before this was true, residency could fail a run that suspending would not have** (a hold ran through the ceiling and ended in `execution timed out`; after a hold, the work after the wake had only what was left). Both are gone. A **cancelled context** ends a held run at once, whether or not its limits are paused (the row above). **A terminate, a cancel or a reclaim recorded in the store** bumps the run's generation, and this worker learns that from its heartbeat, which can be up to `--heartbeat` (5 s by default) late; a held run whose timer fires before then **would have woken and acted**, which a suspended run cannot do, because it has no claim to win. Measured on a real worker (Postgres, real wasmtime, an 8 s sleep, the same worker with residency off as the control): with the heartbeat withheld past the timer, a terminated held run reached the call after its sleep, 6 to 7 s after the terminate had committed, where the suspended run stopped in under 300 ms and never reached it. So a held run **checks the store before it goes back to the guest** (it runs the worker's heartbeat tick itself, so "lost" means exactly what it means to the heartbeat), every time a wait ends: if it no longer has the run, or cannot find out, the wait suspends and the run is replayed by whoever holds it next. That re-measures as never reaching the call, for terminate and for cancel, with and without the heartbeat having run. It costs one heartbeat tick per wake (one batched query per tenant that has runs in flight; ticks are serialized). What it does not change: the instance stays in memory until the heartbeat or the timer, whichever is first, so a terminated run's memory is held for up to that long.

**What is still refused, and why.** A wait whose limit has **already ended** is not held (the expiry is on its way), and neither is a wait due within **100 ms** of a deadline that **cannot** be paused, which is a deadline on a context the caller supplied (reason `deadline`); either way the wait suspends as it always did. The 100 ms is from a measurement: a hold ended by a cancel was back as a suspension in a median of 0.3 ms and at most 9 ms (6.8 ms under `-race`) over 120 samples of a minimal guest on a loaded machine, so 100 ms is about ten times the worst. It is not measured for a guest with real defers or a large history.

**What bounds a hold now.** Because the limits are paused, the wall-clock ceiling no longer bounds how long a run is held; `--resident-window-cap` (5 minutes by default) does, and so does `--resident-max-runs`. A worker that is draining, or at its memory hard limit, sends every held run back at once.

**Boot validation.** Every `--resident-*` flag is validated when the worker starts **even when `--resident-max-runs` is 0**: a value that cannot work (a cap below the floor, a negative number, a multiplier of 0) stops the worker with a message naming it. The defaults are all valid, so a worker that sets none of them is unaffected.

---

### --resident-memory-budget-mb

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Memory the held runs may add up to, in MB; 0 means `--resident-max-runs` times the dearest measured language |

Each held run is charged a per-language figure: **24 MiB for Go**, rounded up from the 19.6 MiB measured after a guest had allocated and dropped about 256 MiB (a minimal guest is 2.5 MiB).

---

### --resident-window-floor

| Type | Default | Description |
|------|---------|-------------|
| duration | `10s` | The shortest a run may be held for; a wait due within this is always eligible |

**Unmeasured**: the owner's starting point. The design notes that 10x a Go run's tiny replay estimate is shorter than a typical wait, which is why a floor exists at all.

---

### --resident-window-cap

| Type | Default | Description |
|------|---------|-------------|
| duration | `5m` | The longest a run may be held for; a wait due later than this is never held |

**Unmeasured**: the owner's starting point. A held run is still inside `--wasm-wall-clock-ceiling` (5 minutes by default), which is a context timeout around one execution.

---

### --resident-replay-multiplier

| Type | Default | Description |
|------|---------|-------------|
| float | `10` | A wait is held if it is due within this many times the estimated cost of replaying the run, clamped to the floor and cap |

**Unmeasured**: the owner's starting point.

---

### --resident-replay-fixed

| Type | Default | Description |
|------|---------|-------------|
| duration | `500ms` | The fixed part of the estimated cost of replaying a run |

Measured at 0.44 to 0.67 s per claim for a minimal guest.

---

### --resident-replay-per-event

| Type | Default | Description |
|------|---------|-------------|
| duration | `0s` | The per-recorded-event part of the estimated cost of replaying a run |

**Unmeasured, so 0**: until it is measured the window does not grow with a run's history, and every run's window is the floor.

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

### --encryption-key-file-previous

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to a file containing a PREVIOUS base64-encoded AES-256-GCM key, for rolling key rotation |

Read-only: a payload sealed under this key still opens, but every new seal
uses `--encryption-key-file`. Requires `--encryption-key-file` to also be
set — a worker started with this flag alone refuses to start. There is no
`admin.workers`-style gate for this the way tenant-secret rotation has; see
[`docs/how-to/rotate-payload-encryption-key.md`](../how-to/rotate-payload-encryption-key.md)
for the rollout sequence a rolling rotation needs to stay safe.

---

### --encryption-key-file-version

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Version number for the key in `--encryption-key-file` (default 2) |

cleat#3203. Set directly, or leave unset and use
`--encryption-key-file-version-file` instead — not both. Unset, this keeps
the historical hardcoded default (2), so an existing deployment needs no
change to keep its current behavior.

---

### --encryption-key-file-version-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to a file holding `--encryption-key-file-version`'s value |

cleat#3203. Re-read fresh on every `SIGHUP`-triggered reload (see
[`docs/how-to/rotate-payload-encryption-key.md`](../how-to/rotate-payload-encryption-key.md)'s
SIGHUP section), the same way `--encryption-key-file` itself already is.
Mutually exclusive with `--encryption-key-file-version`.

---

### --encryption-key-file-previous-version

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Version number for the key in `--encryption-key-file-previous` (default 1) |

cleat#3203. Same contract as `--encryption-key-file-version` above, for the
previous-key slot.

---

### --encryption-key-file-previous-version-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to a file holding `--encryption-key-file-previous-version`'s value |

cleat#3203. Same contract as `--encryption-key-file-version-file` above,
for the previous-key slot.

---

### --encrypt-sensitive-payloads

| Type | Default | Description |
|------|---------|-------------|
| bool | `false` | Encrypt sensitive payload and error columns with AES-256-GCM |

PostgreSQL only -- refused at startup on any other `--driver`. When enabled, the
worker encrypts these columns before writing them:

- `event_history`: `request`, `response`, `error`, `signal_payload`,
  `child_input`, `new_input`, `plugin_input`, `plugin_output`,
  `promise_result`, `promise_error`, and `payload` (including a
  `child_workflow` event's `child_input`/`payload` -- a separate write path
  that did not encrypt until cleat#2328).
- `workflow_instances`: `input`, `result`, `error_msg`, `error_code`,
  `error_op`, `cancellation_reason`, `query_state`.
- `workflow_signals.payload`.
- `workflow_promises.result`, `.error_msg`.
- `workflow_update_requests.payload`, `.result`, `.error_msg`.
- `workflow_schedules.input`.
- `idempotency_keys.error_msg`.

Encryption is per-tenant (AES-256-GCM, HKDF-derived key, tenant ID as AAD),
using the key from `--encryption-key-file`, which must also be set
(cleat#2312; the owner's ruling on the issue is tracked there as "3A").

`ListWorkflows`'s `InputContains` and `ErrorContains` filters are refused
outright under this flag (`ErrSearchUnavailableUnderEncryption`), rather than
silently running a substring search against ciphertext and reporting a false
"no results" -- there is no supported way to search these columns while
encryption is on.

`cleatctl reseal-payloads` (key rotation) re-seals `event_history` and,
since cleat#3241, the six tables above -- `workflow_instances`,
`workflow_signals`, `workflow_promises`, `workflow_update_requests`,
`workflow_schedules`, `idempotency_keys` -- reported as their own sections
in the command's output.

**A worker started without this flag refuses to start if this database has
ever had it enabled (cleat#2324).** A sealed column carries no envelope or
version prefix (by design -- see `engine/encryption.go`'s doc comment), so a
keyless worker cannot recognise ciphertext by inspecting a row; without this
guard it would read a sealed column as plaintext, silently. The database
remembers "payload encryption was enabled here" the first time any worker
starts with `--encrypt-sensitive-payloads` and a key ring configured
(`payload_encryption_ever_enabled`, defined in the baseline schema), and every worker started
without one checks that marker before doing anything else.

The marker is insert-only -- nothing in this release clears it. A deployment
that enabled this flag and later wants to run keyless again has no supported
path back short of operating on `payload_encryption_ever_enabled` directly,
and should not do so without first confirming (e.g. via
`cleatctl reseal-payloads`'s own accounting) that no sealed row remains
anywhere a keyless worker could read.

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
| int | `100` | Number of events before history compaction triggers |

Compaction collapses event history for long-running workflows, retaining only
the compacted state.

---

### --compaction-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `5m` | Interval between compaction checks |

---

## Networking

### --max-stream-readers

| Type | Default | Description |
|------|---------|-------------|
| int | `1024` | Concurrent live SSE readers of `GET /api/workflows/{id}/stream` this worker may hold (`0` = unlimited) |

A reader costs a goroutine, a held HTTP connection and a bounded chunk buffer.
Over the ceiling the route answers `503` with `Retry-After`.

**Not `--connection-budget`.** That budget bounds database pools, and a reader
**holds** no database connection. It is not free of the database, though: each
reader issues one indexed status read per 15-second heartbeat, so the ceiling
also sets a floor on background query load. The two flags are independent and
sizing one from the other will be wrong in both directions.

See [Streaming tokens to a client](../how-to/stream-tokens-to-a-client.md).

---

### --max-body-size

| Type | Default | Description |
|------|---------|-------------|
| int64 | `1048576` (1 MiB) | Maximum request body size in bytes |

General endpoints use this limit.

**Five endpoints do not, and this flag does not move any of them.** There are
four ceilings in total:

| limit | endpoints | moved by |
|---|---|---|
| `--max-body-size`, 1 MiB default | everything not listed below | this flag |
| **64 KB**, fixed | `POST /api/workflows/:id/signal`, `POST /api/workflows/:id/cancel`, `POST /api/workflows/:id/update/:name` | nothing |
| **10 MiB**, fixed | `POST /api/definitions` (WASM upload) | nothing |
| **1 KB**, fixed | `POST /api/dead-letters/:id/terminate` | nothing |

This line named only "signal endpoints" until cleat#1332, so cancel was
undocumented — and cancel is the one most likely to be reached in practice,
because its field is a free-text `reason`. It then said "which of the two it
is" until cleat#1338 established that there were four.

**Every one of them answers an oversized body with `413`**, naming the limit
and the knob, so a request that fails does not need this page to explain
itself:

    {"error":"request body too large: the limit is 1048576 bytes, set by --max-body-size"}
    {"error":"request body too large: the limit is 65536 bytes, fixed for the signal, cancel and update endpoints and not changed by --max-body-size"}
    {"error":"request body too large: the limit is 10485760 bytes, fixed for the definition upload endpoint and not changed by --max-body-size"}

Seven of the sixteen bounded endpoints used to answer `400 {"error":"invalid
JSON: http: request body too large"}` instead — a status asserting the body was
malformed, on a body that was never read. cleat#1338 made all sixteen agree;
that is a status-code change for those seven, and clients branching on `400`
for an oversized body need to handle `413`.

---

### --plugin-max-body-size

| Type | Default | Description |
|------|---------|-------------|
| int64 | `1048576` (1 MiB) | Maximum request body size in bytes for plugin HTTP routes |

Separate from `--max-body-size` above, which bounds only the core API.
Plugin routes got no request-body limit at all until cleat#2232 — thirteen
plugins read `r.Body` directly with `io.ReadAll` or `json.NewDecoder`, and
two of the resulting endpoints are exempt from tenant auth by design
(`POST /slack/interactive`, `POST /ingest/{source_id}`), so an unauthenticated
caller could make the worker allocate an unbounded amount of memory.

Every plugin route is bounded by this flag unless it declares its own,
larger ceiling at registration (`plugin.MaxBody`) — blobstore's
`PUT /blobs/{key...}` does this, sized by its own `max_blob_size` config
(default 10 MiB), since a 1 MiB default would refuse an ordinary blob
upload. An oversized body answers `413`, naming the limit and the knob that
moves it, the same shape `--max-body-size` uses:

    {"error":"request body too large: the limit is 1048576 bytes, set by --plugin-max-body-size unless this route declares a larger one"}

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

### --worker-service-name

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Headless Kubernetes Service (clusterIP: None) selecting this worker's pods |

When set, the worker publishes `<hostname>.<this>` — the per-pod DNS name the
headless Service gives it — in `admin.workers.address`, so another worker can
reach it **by name** rather than by a pod IP that changes on every restart.

This is the address half of the reaper-to-worker veto channel
([cleat#2196](https://github.com/cleat-team/cleat/issues/2196)), and it has to
be set on **every** worker, not only the ones answering: each worker both asks
and may be asked. Empty (the correct value outside Kubernetes, or inside it
without this headless Service) means "my address is not known to be resolvable",
and this worker's runs are reclaimed on the ordinary timeout with no veto
attempted. It is wired into `k8s/deployment.yaml` and the Helm chart by default.

### --internal-addr

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Listen address for `/internal/holds`, reached by other workers only |

Shaped exactly like `--pprof-addr`: its own opt-in address, empty by default,
never served on the ordinary API port and never wired into ingress. When set,
the worker answers `GET /internal/holds/{run_id}?generation=G` from its **own
in-process** map of what it is executing — never by re-querying the database,
because the case this exists for is a worker whose own path to the database has
degraded, where a database query is exactly as blind as the heartbeat the reaper
already does not trust.

Authentication is `CLEAT_INTERNAL_AUTH_KEY`, an **environment variable, not a
flag** (a flag value is visible in `ps`), whose value every worker in the
deployment shares. The secret is read once at startup and used for both halves
of the channel, so a worker needs it set whether it answers this listener or
only asks. `--internal-addr` set without the variable refuses to start rather
than serving a listener that would authenticate no caller.

This is a **narrow, interim** mechanism, symmetric (every worker is both caller
and callee) and tenant-blind (a hold query names a run and a generation, nothing
about a tenant). It is meant to be retired once
[cleat#2169](https://github.com/cleat-team/cleat/issues/2169)'s general
operator-credential design lands — it is deliberately not that.

## The reaper's veto channel

With the two flags above set, `reapOnce` asks the worker named in a stale row
whether it still holds that run before reclaiming it, and a `"yes, and I am
making progress"` defers that row by **one extra reclaim window** — no more.
The database stays the only authority on who holds a run: the channel never
grants ownership, and generation fencing is unchanged.

**The veto is bounded, and it costs liveness in exactly one direction.** A row
stale by one reclaim window is asked; a row stale by **two** is reclaimed
without being asked at all, whatever its holder would have said. So an honest
holder — or a lying one — can delay its own run's reclaim by one window and no
further.

**Every failure to get a definite "yes" is a reclaim.** A worker that is
unreachable is treated exactly as one that is dead: a dial error, a timeout, a
non-200, a malformed answer, or no published address all mean the row is swept
on the ordinary timeout. That is deliberate — the alternative strands a
genuinely dead worker's runs forever — and it is why the channel is an
optimisation over fencing rather than a new source of truth.

One consequence worth knowing when tuning: a worker's row in `admin.workers` is
retained for **longer than the membership lease** (`max(2 × --heartbeat, 10s)`)
— long enough to cover the whole window in which its runs can still be asked
about, since the workers this resolves are precisely the ones whose heartbeats
have stopped. The connection share is unaffected: it is still divided by the
lease.

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

### --wasm-module-cache-max-mb

| Type | Default | Description |
|------|---------|-------------|
| int | `512` | Max **estimated** size of the compiled-module cache, in MB (LRU eviction) |

**Two different caches, and this is the larger one.** `--wasm-cache-max-mb`
above bounds the WASM *byte* cache — the artifacts as uploaded.
`--wasm-module-cache-max-mb` bounds the compiled *native code* wasmtime
produces from them, which measures 2–3× larger for a release build.

This is the bound to size a deployment against. Its sibling,
`--wasm-module-cache-max-entries`, cannot say what a hundred modules cost —
measured on cleat's own artifacts, a hundred entries was **8.6 MB of
AssemblyScript or 4.6 GB of Python** when this was measured (AssemblyScript has
since been removed from cleat; see the historical row below), a ~500× spread in
what one flag value meant. Both bounds apply: the entry count still bounds map and list overhead
independently of artifact size.

**Estimated, not measured**, and every layer says so. A compiled
`wasmtime.Module` exposes no cheap size, and `Serialize()` would cost a
serialisation plus a transient allocation the size of the module — 46 MB for
the Python component — to learn a number the input length already predicts. So
each entry is costed at **4× the wasm it came from**.

| artifact | wasm | compiled | ratio |
|---|---|---|---|
| widget-store (AssemblyScript, **historical**) | 9.5 KB | 86 KB | 9.1× |
| rust-workflow (release) | 149 KB | 458 KB | 3.1× |
| java-workflow | 299 KB | 694 KB | 2.3× |
| hostcallsjava | 506 KB | 1.04 MB | 2.1× |
| **call_all_plugins (Python)** | **19.3 MB** | **46.0 MB** | 2.4× |
| rust-workflow (debug) | 5.2 MB | 1.1 MB | 0.2× |

Release builds sit in 2.1×–3.1×, and the multiplier is **4** rather than 3 so
that it clears the measured maximum instead of approximating it — an estimate
that *under*-counts would let the cache hold more than this bound says, which
is the one failure it cannot tolerate. The cost is that a typical artifact is
over-counted by about a third, so a given value holds correspondingly fewer
modules than raw arithmetic suggests.

The AssemblyScript row is a historical measurement: cleat no longer supports
that language and the module is not rebuilt for this table. It is kept because
it is the measured example of a tiny module running high.

Both outliers fail safely: tiny modules (that row was one) run high but cost tens
of kilobytes, and debug builds run low so the estimate over-counts them
further. Over-counting shrinks the cache; it cannot overrun the bound.

Observe it with `cleat_wasm_compiled_module_cache_bytes`, beside the entries
gauge.

One module larger than the whole bound is **kept**, not evicted on insert —
otherwise the cache would compile a large artifact, drop it, and recompile it
on every call.

---

### --wasm-memory-max-mb

| Type | Default | Description |
|------|---------|-------------|
| int | `256` | Max WASM linear memory per module in MB |

Corresponds to 4096 WASM pages at 256 MiB (64 KiB per page). It is a **ceiling, not a reservation**: a guest may grow up to it and no
further, and a small workflow pays nothing for a higher number. Enforced by the wasmtime store limiter; set to `0` to use the built-in
default (`DefaultWasmtimeMemoryLimitBytes`, `engine/wasmtime_options.go`). Lowering it limits the memory one workflow can take; raising it
allows workflows with larger memory requirements to execute.

The value applies to every language as given, including a value too small for the guest. A .NET (C#) guest cannot start below 52 MiB
(measured on a hello-world); under that it dies at startup with `.NET runtime failed to initialize`, which names neither the limit nor the cause.

**Changed in cleat#4171 (owner decision, 2026-11-19): the default was 32 MiB and is now 256 MiB.** The 32 came from the old wazero default with
no sizing rationale. Until then, a stock worker ran on 32 MiB and a per-language floor (`languageMemoryFloorBytes`) raised it to 64 MiB for
C# only when the flag was not passed; the default now clears every runtime's minimum, so the floor, and the worker's practice of passing a
limit only when the flag was set, were removed. A worker running many workflows at once can in the worst case use this much for each of them,
so size `--concurrency` and the machine's memory together. `--memory-soft-limit` stops claiming new work at a fraction of memory. It sees a guest's memory when the worker reads cgroup v2 `memory.current` or `/proc/meminfo` (host-wide, so it also counts everything else on the machine), and is blind only on its last-resort Go-heap source, which macOS falls back to (cleat#4196).

---

### --wasm-instruction-limit

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max WASM instructions per invocation |

Limits the number of WASM instructions a single workflow invocation can
execute. Set to `0` for no limit. **Enforced via wasmtime fuel**
(`SetConsumeFuel`/`SetFuel`) — see the flag's own help text in
`cmd/cleat-worker/config.go`. This said "a wazero function listener" until
2026-09-06. That mechanism is real but is not what a worker uses: `fuelMeter`
in `engine/runtime.go` charges one unit per *function entry* on the wazero
runtime, which is why wazero cannot fence a compute-bound guest at all — a
tight loop inside one function never enters another.

---

### --wasm-instance-timeout

| Type | Default | Description |
|------|---------|-------------|
| duration | `30s` | Max guest execution time for one WASM invocation |

Bounds the time a guest spends *executing* (a fresh execution or one replay
pass) before it is forcibly interrupted, via wasmtime epoch interruption, which
stops even a tight loop that never calls the host. Time blocked in a host call
is not charged; `--wasm-wall-clock-ceiling` bounds that. `0` (or unset)
uses the default: **the timeout cannot be turned off**, because the engine reads a
value of 0 or less as the default (pinned by `TestAZeroInstanceTimeoutIsTheDefaultNotDisabled`).

**A value above `0` and under `250ms` is refused: the worker prints the
reason and exits non-zero before it serves anything.** Below about 200ms the
hard epoch trap beats the soft kill to a poll, so the defers of a workflow
this timeout stops do not reliably run. Measured: at 100ms the soft kill
reached the guest 0 times in 20 runs, at 150ms 18 of 20, and at 200ms and 240ms
20 of 20 (`ABI.md`, "There is a cliff below about 200 ms"; cleat#3922). 250ms
is the floor because it leaves headroom over that measurement. Earlier releases
accepted any value here, so a deployment that sets this flag low must raise it
before upgrading.

A tenant's own `wasm_instance_timeout_ms` (`cleatctl set-tenant-setting
--wasm-instance-timeout-ms`) is held to the same floor: a value above 0 and
under 250 is refused with the same explanation, and nothing is written. A value
a tenant already has under 250ms is not rewritten and not newly refused, it is
used as it stands (`min(run, tenant, operator)`), so it still reaches the cliff
until an operator raises it; it also does not block changing that tenant's other
settings. To find them:

    SELECT tenant_id, wasm_instance_timeout_ms FROM tenant_settings
    WHERE wasm_instance_timeout_ms > 0 AND wasm_instance_timeout_ms < 250;

(Run it as a role that is not subject to row-level security; on MySQL it is the
control-plane database, on SQL Server `dbo.tenant_settings`.) The table's CHECK
only requires the value to be positive, so a direct SQL write can still store a
low one.

A run's own `wasm_instance_timeout_ms` on the start request (`POST /api/workflows/<name>/start`) is
held to the same floor: a value above 0 and under 250 is a `400`. See "Per-run limit overrides on a
start request" in `docs/reference/workflow-lifecycle.md`.

Not covered, and still reaching the cliff as ABI.md documents: a direct SQL write to
`tenant_settings` or `workflow_instances`, the engine option a library user sets directly, a context
deadline, and the epoch tick.

---

### --wasm-output-buffer-size

| Type | Default | Description |
|------|---------|-------------|
| int | `1048576` (1 MiB) | WASM output buffer size in bytes |

For a **core-module** guest's direct export call, the host places an input area and an output area, each this size, past the end of the guest's
memory and grows the memory to fit (so each such call needs twice this value of headroom under `--wasm-memory-max-mb`); a result larger than the
output area fails with `output overflow`. Python and C# components take the value and do not use it.

**Changed in cleat#4171 (owner decision, 2026-11-19): the default was 32768 (32 KiB) and is now 1 MiB**, the engine's own default
(`DefaultOutBufSize`, `engine/memory.go`) and the figure `ABI.md` states. (An earlier version of this page, until 2026-08-09, said `1048576`
while the flag was `32768`; the flag, the engine and this page now agree, and a test fails if they stop agreeing.)

---

### --wasm-max-string-len

> Changed with cleat#4130: the default is now `2097152` (2 MiB), the same constant as the engine package's `DefaultMaxWasmStringLen`. It was
> `65536` (64 KB) here and 1 MiB in the engine package, and the 64 KB one is what a running worker used. A test pins the two together.

| Type | Default | Description |
|------|---------|-------------|
| int | `2097152` (2 MiB) | Maximum length, in bytes, of any one string a guest hands the host |

Bounds every string a workflow passes to a host call (a request, a child input, a signal or log payload, a fetch body, a side-effect value),
for **every** guest language: Go and Rust core modules and Python and C# components alike. A call over the limit is **refused**, not truncated:
as an error classified invalid-request for a call that can return one, and as a bad-parameter result or a trap for a call that cannot. A
refused call runs nothing on the host and records no event. Raise the flag if a workflow legitimately sends more. The limit is a per-argument
bound on what the host will copy out of guest memory; responses are bounded separately by `--wasm-output-buffer-size`.

---

### --wasm-max-response-bytes

| Type | Default | Description |
|------|---------|-------------|
| int | `2097152` (2 MiB) | Maximum size, in bytes, of one host call's response, for component guests (Python, C#) |

Added with cleat#4132. A component guest used to receive at most 64 KiB of any host call's response (a service reply, a plugin result, a fetched
body), however large the host's was, so a Python or C# workflow could not receive an LLM reply or a document that a Go workflow could. The host now
sizes the buffer to the response up to this ceiling, so a 200-byte reply costs 200 bytes. **Over the ceiling the call fails, visibly:** call-error
code 7 (output truncated), with the prefix that fit as its message; the call has already run and recorded its effect, so it is not retried.

The Go and Rust SDKs size their own buffers, start at 64 KiB and double on each reported truncation, and are **compiled with the same 2 MiB
ceiling** (`_cleatOutBufCeiling` in the generated adapter, `OUT_BUF_CEILING` in `cleat-sdk`). Raising this flag above 2 MiB therefore does not
raise what a Go or Rust workflow can receive until it is rebuilt against a larger constant: no guest-reported length is available to grow to. The
call that truncates cannot be rescued, so the first oversize response on a Go or Rust workflow fails and the buffer is larger for the calls after
it. The value is capped at 16 MiB - 1, which is what a durable-call result word can carry; a larger setting is rejected at startup.

**One response stays small in every language:** an `await_signals` payload is carried in a 16-bit length field of its result word, so it is at most 65,535 bytes whatever this is set to.

This is **not** `--wasm-output-buffer-size`, which sizes the entry-point input and result regions in the module's linear memory.

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
| int | `30` | Days after which a **failed** workflow's event history is deleted (0 disables) -- a **done** workflow's is already gone at finalize, so this flag only does first-hand work for `failed` |

Deletes `event_history` rows for terminal workflows, but in practice that
means `failed` ones: a `done` workflow's history is purged immediately at
finalize (`finalize_workflow_status`'s `done` branch), before this sweep ever
runs, while a `failed` workflow's history is untouched until this flag's
window elapses -- `store.FailWorkflow` never purges it (cleat#1973). The
`workflow_instances` row itself (status, result, error, def_name) is
untouched by this flag either way -- see `--completed-workflow-retention-days`
below to also reclaim that. See `docs/operations/workflow-retention.md` for
the full story, including `terminated` and `dead_lettered`.

---

### --completed-workflow-retention-days

| Type | Default | Description |
|------|---------|-------------|
| int | `0` (disabled) | Days to retain `workflow_instances` rows for terminal workflows (done/failed/terminated/cancelled) before permanently deleting them |

Unlike `--retention-days`, this deletes the workflow's own record, not just
its step-by-step event history: after this runs, a purged workflow no longer
appears in `ListWorkflows` or the admin dashboard, and its outcome (result,
error, status) is gone. Off by default -- an operator opts in after deciding
how long their own audit/compliance requirements need a workflow's outcome
retrievable. `dead_lettered` workflows are never affected by this flag; they
have their own deletion path, `--dead-letter-retention-days`, which cleat#1023
wired and which is also off by default. (This sentence said "currently unwired"
until cleat#1315 noticed it while adding the flags below.) On the Go SDK a
workflow reaches that state only by exhausting a retry policy short enough to
have run on the host (see `cleat.hostRetryBudget`); a long-backoff policy
retries via durable sleep and produces a terminal error the worker's
dead-letter predicate does not match. See
`docs/operations/workflow-retention.md` and IMPROVEMENT-PLAN.md 3.88.

Any remaining `event_history` for a purged workflow is deleted in the same
pass. See `docs/operations/workflow-retention.md` for the full design
(default rationale, FK/cascade behavior per dialect, batching, metrics).

**`cancelled` is in the list, and that is deliberate.** All three dialects spell the same predicate
(`engine/retention_predicates.go`) —

```
WHERE status IN ('done', 'failed', 'terminated', 'cancelled')
```

— and a comment beside it gives the reason: an operator who has opted into collecting terminated
runs expects to collect cancelled ones too, since both are imposed by a person on a run that did not
finish on its own. The description above said `(done/failed/terminated)` until 2026-09-25, omitting
`cancelled`; the predicate, not the description, is authoritative.

---

### --dead-letter-retention-days

| Type | Default | Description |
|------|---------|-------------|
| int | `0` (disabled) | Days to retain `dead_lettered` `workflow_instances` rows before permanently deleting them, along with their `event_history`, signals and promises |

**A dead-lettered run has its own lifecycle and its own knob, deliberately.** It is the run an
operator most wants to inspect afterwards, so it is not swept up with completed work:
`--completed-workflow-retention-days` never touches a dead-lettered run, and this flag never touches
one that is not dead-lettered. Off by default, on the same reasoning as
`--completed-workflow-retention-days` above — it deletes the record itself, not just the history.

**Retention here is about the row, not about membership.** A dead-lettered run leaves the dead-letter
queue through a redrive verb — `POST /api/dead-letters/:id/retry` or `.../reprocess` — or through
`POST /api/dead-letters/:id/terminate`; this flag is what eventually removes the record of a run
nobody acted on. See `docs/operations/workflow-retention.md`, and
`docs/reference/workflow-lifecycle.md` → *Outcomes* for what each exit commits to.

---

### --version-gc-interval

| Type | Default | Description |
|------|---------|-------------|
| duration | `0` (disabled) | Interval between automatic workflow-version garbage collection sweeps |

**This is the switch, and it is off.** Version GC permanently deletes workflow
*definitions*. An in-flight instance whose version has been collected cannot
find the WASM binary to replay against — the module cache is keyed by
`def_name:def_version`, so the failure lands on a running workflow rather than
at the point of deletion. That is materially more destructive than clearing
compaction state, which is why `--retention-days` ships on at 30 and this ships
off, on the same reasoning `--completed-workflow-retention-days` records above.

With this unset, GC still runs when a person invokes it — `cleatctl versions gc`
or `POST /api/versions/gc` — and those take their own policy overrides. Before
cleat#1315 those two were the *only* surfaces and neither could change the
policy.

Unlike `retentionLoop`, this loop is **tick-first**: it does not sweep before
its first tick. Retention pre-runs because a deploy cadence under its 24-hour
period would disable it entirely (cleat#1002); that argument does not transfer
to an opt-in destructive sweep, where a pre-run would make every worker restart
delete definitions immediately and turn a rolling deploy into a burst of sweeps.

Every pass logs `version gc swept` with the policy it used, **including passes
that remove nothing** — so "ran and found nothing" and "disabled" are
distinguishable in the log.

**Which tenants it reaches.** With this set, a worker run with
`--claim-across-tenants` collects old versions of **every tenant it serves**, each
through that tenant's own store and under that tenant's row-level security, with
the same policy (cleat#3708). Before that only the worker's own tenant's
definitions were collected, so every other tenant kept every old version. A
version with a running instance is kept whichever tenant it belongs to. The
other tenants' passes log one extra line, `version gc swept other tenants`, when
they removed something. Three limits worth knowing:

- **The definition statements behind a pass are scoped to the store's own tenant in the SQL
  itself (cleat#3729), not only by row-level security.** The collection groups
  definitions by workflow name, so this is what keeps two tenants' same-named workflows
  apart on a connection the policy does not apply to (a superuser, or a role with
  `BYPASSRLS`; an operator's `--db` DSN commonly is one). Before #3729 such a connection
  saw every tenant's definitions in every pass, and one pass could delete another
  tenant's versions while keeping its own: on a release that predates it, do not set
  `--version-gc-interval` on a worker whose connection bypasses the policy. SQL Server
  has no such principal, and already carried the predicates.
- MySQL is single-tenant and a sharded store cannot list tenants, so for both this
  is unchanged: the worker's own tenant only.
- `POST /api/versions/gc` and `cleatctl versions gc` stay scoped to the tenant that
  made the call. A request made as one tenant must not delete another's definitions.

---

### --version-gc-min-versions

| Type | Default | Description |
|------|---------|-------------|
| int | `3` | Minimum recent versions retained per workflow, regardless of age or activity |

Applies to the scheduled sweep. `cleatctl versions gc --min-versions=N` and
`POST /api/versions/gc?min_versions=N` override it per invocation.

**`0` is refused** on both manual surfaces. `engine.GarbageCollectVersions`
treats a non-positive value as *unset* and substitutes the default, so accepting
0 would run under a policy of 3 while reporting success.

---

### --version-gc-max-age

| Type | Default | Description |
|------|---------|-------------|
| duration | `720h` (30 days) | Age at which a **deprecated** version becomes eligible for collection |

A version that is not deprecated is never collected, whatever its age.
`cleatctl versions gc --max-age=DURATION` and
`POST /api/versions/gc?max_age=DURATION` override it per invocation.

Go duration syntax, so `720h` rather than `30d` — and a bare number is refused:
`--max-age=7` is seven *nanoseconds* to Go, not seven days.

---

### --redact-patterns-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to file with custom redaction patterns (one per line) |

When set, the worker reads patterns from the specified file and redacts the
value of any field whose name contains one of them, in addition to the built-in
matching. Each line is one pattern: a plain substring (not a regular
expression), matched case-insensitively anywhere in the field name. Blank lines
and lines starting with `#` are ignored. Unlike the built-in `token`, which is
read as a whole word, a custom pattern matches inside any word of the key, so
`key` redacts `monkey`; name the fragment you mean. Leave empty to use only the
built-in defaults.

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
| int | `50000` | Max events one run may write before the engine continues it as new (0 disables the bound) |

Bounds how much history a single **run** may write. Exceeding it is a
**rollover, not a failure**: the durable call is refused before it is
dispatched, so no side effect happens; the guest sees an error and unwinds
through its entry-point wrapper, draining its defers as it would for an
explicit `ContinueAsNew`; and the executor records a `continue_as_new`
suspension. The next run starts with a reset event count and makes the refused
call for real.

`--retention-days` bounds history for *terminal* runs. A runaway is not
terminal, so that sweep never reaches it — this is the only bound that does.

Setting `0` disables the bound and restores the pre-cleat#1829 behaviour, in
which one looping workflow can fill `event_history` with nothing to stop it.

> This section said the default was `0` and that "the workflow is terminated
> with a quota error". Both were wrong, and the second was wrong in the
> direction that argues against ever setting the flag: an operator reading it
> would conclude a runaway gets killed. `engine/callerrors.go`'s
> `eventCapCallError` says the opposite — "a refusal, not a failure" — and
> `engine/durablecalls.go` auto-triggers `ContinueAsNew`. The default changed in
> cleat#1829 and this reference did not follow it, because nothing compares a
> flag's default against this file.

---

### --max-quota-children

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max child workflows per workflow (0 = unlimited) |

Limits the number of child workflows a single parent workflow can spawn.
When exceeded, further child start attempts fail with a quota error.

**Deliberately unbounded by default**, unlike `--max-quota-events`. Exceeding
this one *fails* the workflow rather than rolling it over, so any default would
turn working deployments into failing ones at whatever number was chosen, and
there is no usage data to choose from. cleat#1829.

---

### --max-quota-concurrency-keys

| Type | Default | Description |
|------|---------|-------------|
| int | `0` | Max concurrency keys per workflow (0 = unlimited) |

Limits the number of distinct concurrency keys a single workflow can register.
When exceeded, further key registrations fail with a quota error.

**Deliberately unbounded by default**, for the same reason as
`--max-quota-children`: exceeding it fails the workflow. cleat#1829.

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

### --max-priority-magnitude

| Type | Default | Description |
|------|---------|-------------|
| int | `1000` | Bound on a caller-supplied workflow `priority`, in either direction (0 disables the bound) |

`priority` arrives in the start request body, is stored as a bare
`INTEGER NOT NULL DEFAULT 0`, and the dispatch claim orders
`priority ASC, created_at` -- **lower runs sooner**. Nothing narrowed the value,
so the whole int32 range was reachable by any caller who could start a workflow.

Within one tenant that lets a caller put one run permanently in front of its own
queue. With [`--claim-across-tenants`](#--claim-across-tenants) the same ordering
is **global**, so `priority: -2147483648` takes the front of every tenant's work
indefinitely -- a caller deciding the order for a database they share.

A request outside `-N..N` is **refused with 400**, not clamped. Zero already
means "the default", so there is no value a clamp could substitute that means
"the number you asked for was not available"; the refusal says what the bound is.
This matches the per-run limit overrides decoded a few fields away, which refuse
a negative rather than treating it as absent.

**The bound is symmetric on purpose.** A negative priority is a supported way to
put work ahead of the default without renumbering everything already at `0`, and
cleat#1051 exists because a hand-rolled parser made negative values
unexpressible. A floor of zero would close this hole by removing that feature,
so the bound constrains *magnitude* rather than sign.

Set `0` to remove the bound and restore the full int32 range.

## Multi-Tenancy

### --claim-across-tenants

| Type | Default | Description |
|------|---------|-------------|
| bool | `true` | Execute work for every tenant, not only this worker's own |

A worker holds one store, scoped to one tenant. With this off, its dispatch
loop claims through that store, and the claim only ever returns rows for that
one tenant -- enforced by row-level security on PostgreSQL and SQL Server, and
by an explicit `tenant_id` predicate on MySQL -- which means a non-default
tenant's workflows never execute.

**This defaults to `true`**, and did not always. The only mechanism used to be
`admin.claim_workflows`, whose owner needs `BYPASSRLS` — a privilege only a
superuser can grant, and one managed PostgreSQL cannot grant at all. Turning
that on had to be a deliberate act, so it was off.

[The per-tenant mechanism](#how-cross-tenant-work-is-served) asks nothing of the
deployment: it reads the tenant list from `admin.tenants`, which carries no
row-level security, and then does the per-tenant work under each tenant's own
RLS context. Nothing is exempt from a policy and nothing has to be granted — at
which point leaving a non-default tenant's work unexecuted by default stopped
being caution and became a surprise.

**With one tenant this costs one query per tick** — the tenant list — and
nothing else changes. Set it to `false` to hold a worker to its own tenant.

Their **schedules** are the other half, and this flag covers both. The firing
loop reads due schedules tenant by tenant, each read under that tenant's own RLS
context, and fires each schedule through a store scoped to the schedule's own
tenant -- so a non-default tenant's cron fires, and the run it starts is recorded
under that tenant.

With this set, nothing is ever read across tenants: the claim itself is made
under the claimed tenant's context, and each claimed workflow executes against
a store scoped to its **own** tenant. Event history, state, child workflows and
schedules are tenant-scoped throughout.

**Where rotation is not available**, the worker keeps claiming its own tenant
and says so (see the warning below): on **MySQL**, whose store cannot enumerate
tenants, and on a **sharded** worker (`--shards-file`), for the same reason. The
flag is accepted there and does nothing.

**It needs no grant for either half.** The table below is what the *retired*
widened query required; the mechanism that replaced it requires none of it —
not for the claim and not for the due-schedule read. It is kept because a
deployment may still carry those grants, and because `admin.in_flight_workflow_ids`
(`migrations/postgres/001_schema.sql`'s `ALTER FUNCTION ... OWNER TO
cleat_dispatcher`, the plugin sweep) still uses the same `cleat_dispatcher` role.

| dialect | what the deployment must do |
|---------|-----------------------------|
| PostgreSQL | **Nothing, for either half.** A fresh deployment needs neither migration: rotation reads the tenant list from `admin.tenants` (which carries no row-level security) and then does the per-tenant work under each tenant's own RLS context, granting nothing special. A deployment that already applied `023_cross_tenant_claim.sql` (creating `cleat_dispatcher`, `NOLOGIN BYPASSRLS`) keeps it, because `admin.in_flight_workflow_ids` (`migrations/postgres/001_schema.sql`'s `ALTER FUNCTION ... OWNER TO cleat_dispatcher`, the plugin sweep) still uses that role -- but the role is no longer required for the claim or the due-schedule read, and `024_cross_tenant_schedules.sql` is not required at all. |
| SQL Server | **Nothing, for either half.** Rotation reads the tenant list from `admin.tenants` and then does the per-tenant work under each tenant's own session context, so it runs against the shipped `dbo.fn_tenant_filter`, which does not consult `cleat_admin`. Measured on SQL Server 2022 by `cmd/cleat-worker/a_rotating_claim_serves_every_tenant_test.go` (cleat#3446), connecting as `sa` (mapped to `dbo`), with the shipped predicate: every live tenant's runs were claimed once, a suspended tenant's were not, and no claim left its tenant. That the policy applies to that principal is a separate measurement, made directly by a subtest added in cleat#3452 (a tenant-B run is invisible under tenant A's session context and visible under B's); the claim results alone do not show it, because the SQL Server claim carries its own `tenant_id` predicate. That used to need `migrations/mssql/optional/cross_tenant_claim.sql` plus membership of `cleat_admin` (cleat#1541, before cleat#1926); it needs neither now, and [the optional migration](#why-sql-servers-opt-in-exists-and-what-it-still-does) is not a prerequisite. What this does not cover: a least-privilege application login's ordinary object grants (`SELECT`, DML, `EXEC`), which the test did not exercise. |
| MySQL | **Not available: the worker claims its own tenant only.** `MySQLStore` cannot enumerate tenants (it does not implement `engine.TenantLister`), so rotation is unavailable on any MySQL worker, and the worker logs the warning below once and claims its own tenant. `MySQLStoreFactory` also gives each tenant its own physical database (`cleat_` plus the tenant id with its dashes turned into underscores), so there is no predicate to drop -- the other tenants' rows are not filtered out, they are in another database. This row used to say that a MySQL deployment pointed at a *single shared* database works; it does not, for the first reason, whatever the topology. Pinned by `TestARotatingClaimOnMySQLFallsBackToTheWorkersOwnTenant`. |

If the flag is set but the store cannot claim across tenants -- MySQL, a sharded
worker, or no per-tenant store factory -- the worker logs one warning naming the
reason and keeps claiming its own tenant. It does not stop claiming, and it does
not fail to start: on a mixed fleet the flag says what the operator wants while
the store says what is actually possible, and those can disagree.

A worker that cannot rotate is therefore narrowed rather than stopped. No grant
is involved on PostgreSQL or SQL Server, so a missing grant is no longer a
reason for it.

### How cross-tenant work is served

Not a flag — there is one mechanism, and this is what it does.

Both loops use the same two-phase method: read the tenant list from
`admin.tenants`, then do the per-tenant work through a store scoped to that
tenant.

**The dispatch claim** polls tenants in turn, each getting a bounded share of
the batch, running the **same** single-statement `FOR UPDATE SKIP LOCKED` claim
the single-tenant path runs, under that tenant's own context. Isolation is the
row-level-security policy on PostgreSQL, and an explicit `tenant_id` predicate
in the statement on SQL Server (which the policy backs up).

**The due-schedule read covers every tenant on every tick**, and that difference
is deliberate. Work that waits a tick is work that waits a tick; a cron schedule
that waits a tick has *fired late*, and with enough tenants a minutely schedule
quietly becomes an every-few-minutes one. The schedule loop runs on a 15-second
ticker against cron whose finest granularity is a minute, which is what makes a
full pass affordable — roughly a second of a fifteen-second tick at a thousand
tenants. At tens of thousands on one worker the answer is more workers with
fewer tenants each, not a shorter pass.

It needs **no database-side grant**, because `admin.tenants` carries no
row-level security and `cleat_app` already holds `SELECT` on it. Every read of
`workflow_instances` and `workflow_schedules` still happens under some tenant's
own RLS context.

That is what makes multi-tenant dispatch possible on **managed PostgreSQL**.
`BYPASSRLS` can only be granted by a true superuser, and RDS, Cloud SQL and
Azure do not have one — AWS documents the RDS master role as
`LOGIN NOSUPERUSER INHERIT CREATEDB CREATEROLE`.

It is also **fair**. The mechanism it replaced ordered
`priority ASC, created_at` across every tenant at once, so the tenant holding
the oldest rows won every slot until its backlog drained. Now a tenant with
10,000 queued runs takes its share of each batch and no more, and the rotation
cursor — worker-local, no shared table — resumes past the tenants it served so
the rest are reached on later ticks.

> **Retired: `--claim-strategy`.** It selected between this and
> `admin.claim_workflows`, the older single widened query behind a `BYPASSRLS`
> role. That existed to keep the old mechanism reachable while this one proved
> itself. The flag is gone, and so is the path it selected. `admin.claim_workflows`
> itself was dropped from the PostgreSQL schema (cleat#3444); `admin.get_due_schedules`
> is still defined and nothing calls it. Re-derive:
> `git grep -c 'admin\.claim_workflows' -- migrations/postgres/001_schema.sql` prints
> nothing, and `git grep -n 'admin\.get_due_schedules' -- '*.go' ':!*_test.go'` finds
> only comments.

### --claim-tenants-per-tick

| Type | Default | Description |
|------|---------|-------------|
| int | `16` | The most tenants one dispatch tick will poll when claiming across tenants |

The rotating claim trades one query per tick for one enumeration plus up to *k*
claims, so *k* needs a ceiling that is not the tenant count: a deployment with
10,000 tenants must not issue 10,000 queries per second.

Tenants beyond the ceiling are **not skipped** — the cursor resumes past the
ones served, so they are reached on a later tick. This bounds queries per tick,
not which tenants get served.

The default sits above the batch sizes a dispatch tick usually asks for, so in
the common case the limit fills before the ceiling is reached and the ceiling
costs nothing. `0` uses the default.

### Why SQL Server's opt-in exists, and what it still does

**The worker does not need this.** Rotation (above) runs against the shipped
predicate, so applying `migrations/mssql/optional/cross_tenant_claim.sql` is not
a step of enabling `--claim-across-tenants`. What it still does is make a member
of `cleat_admin` exempt from the tenant filter, for an operator connection that
should read across tenants -- `cleatctl`'s RLS check reports such a login as
`exempt` once the optional migration has been applied, and as subject to the
policy, with a note that membership grants nothing, before. It records which
predicate is installed in `admin.rls_predicate_form` (`plain` or `admin`); that
marker is read by `cleatctl`, not by the worker.

The bypass is a disjunction inside the row-level-security predicate:

```sql
WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
   OR IS_ROLEMEMBER(N'cleat_admin') = 1
```

It is free for a query that supplies its own tenant and expensive for one that
relies on RLS to supply it, because the disjunction is what stops the optimiser
pushing a bare equality into an index seek. Measured on SQL Server 2022, 200 000
rows over 200 tenants:

| predicate | query carries `tenant_id = ?` | plan | logical reads |
|---|---|---|---|
| plain (shipped) | yes | Index Seek | 33 |
| plain (shipped) | **no** | Index Seek | 33 |
| with the bypass | yes | Index Seek | 33 |
| with the bypass | **no** | **Index Scan** | **5760** |

So the cost lands on the statements that cannot name a tenant --
`HeartbeatBatchFenced`, which scans by design because one worker legitimately
holds instances across many tenants -- and on any future statement that
forgets to. (Claiming itself no longer scans cross-tenant: #1926 replaced the
old `ClaimWorkflowsAcrossTenants` with unconditional per-tenant rotation, each
claim scoped to one tenant.) Applying the opt-in accepts the scan cost on
every tenant-scoped table, which is why it is a deliberate act rather than the
default.

**Reversing it:** the file this section used to name for that,
`075_the_admin_bypass_is_opt_in.sql`, no longer exists -- `migrations/mssql/`
holds only `001`-`003` since the SQL Server migration compaction -- and the
optional file's own header still cites it. No tested reversal script ships
today, and the file's own text is wrong about it (cleat#3453). Do not improvise one against a production
database: the policies carry BLOCK predicates as well as the filter one, and
recreating the function without them silently drops cross-tenant write
protection.

**The worker says which mode it is in at startup**, before either loop ticks, so
you do not have to infer it from silence:

```
INFO  cross-tenant workflow claim is available by tenant rotation; no database grant is required
INFO  cross-tenant due-schedule read is available by tenant rotation; no database grant is required
```

or, if this worker cannot rotate at all (no per-tenant store factory, or a
store that cannot enumerate tenants):

```
WARN  claim-across-tenants is set but this worker cannot serve other tenants;
      it will claim and fire cron for its own tenant only
      reason=<why the store can't rotate>
```

**There is no longer a state where one loop is available and the other is
not.** #1926 replaced the two migration-gated widened queries (023's claim,
024's schedule read) with one rotation mechanism that both loops share, so one
`rotatingClaimAvailability` check now answers for both -- which is why the WARN
names neither 023 nor 024 and points at nothing to apply. A deployment that
never ran either migration is unaffected either way.

It is a report, not a gate -- refusing to start would contradict the degradation
above, and would turn a store that cannot rotate into an outage for the
worker's own tenant, which was never affected.

**It does not check `BYPASSRLS`.** This paragraph used to describe a PostgreSQL
check for a lost `BYPASSRLS` grant on `cleat_dispatcher` -- that check belonged
to the widened `admin.claim_workflows` query #1926 retired, and went with it:
`reportCrossTenantCapability` (`cmd/cleat-worker/setup.go`) calls only
`rotatingClaimAvailability`, which asks nothing about role attributes. Confirm
with `grep -rn CheckCrossTenantCapability --include='*.go' .` -- the only
non-test hit is `setup.go`'s own comment recording that the function was
retired with #1926; it is not defined anywhere in `engine/`.

---

## Multi-Instance

### --shards-file

| Type | Default | Description |
|------|---------|-------------|
| string | `""` | Path to shards JSON config for multi-shard operation |

**EXPERIMENTAL in 0.6.0** (owner decision D80, 2026-11-13). Sharding is not a supported configuration of this
release; it has known gaps (cleat#3606, cleat#3684, cleat#3724, cleat#3725), listed with the rest of its limits in
[Database sharding](../sharding.md).

JSON format:

```json
[
  {"name": "shard-0", "conn_str": "postgres://...", "tenants": ["tenant-a"]},
  {"name": "shard-1", "conn_str": "postgres://...", "tenants": ["tenant-b"]}
]
```

**The first entry is the admin shard.** A sharded worker uses the first shard's database for plugin migrations
and administration (`db = shardDBs[0]`, `cmd/cleat-worker/main.go`), so an entry's place in the file is not
cosmetic. Today, on that shard:

- plugin migrations run, and plugin tables (event subscriptions, webhook sources and the like) live;
- API keys and tenants are resolved: the worker's key and tenant store is built on that database;
- the plugin-name registry and the override records of a deploy's plugin check are read and written
  (see [Deploying workflows](../how-to/deploy-workflows.md)).

`--migrate-only` and `--migrate-on-start` migrate **every** shard's core schema (cleat#3718), and the plugin
tables only on the admin shard. Before cleat#3718 they migrated the first shard alone and exited 0. Every
shard's migration chain is checked before any is changed, and a shard that cannot be migrated fails the run
naming it. `--migrate-db` names the first shard's migration connection only; the others are migrated through their
own `conn_str`. A normal start checks **every** shard's migration chain and refuses, naming the shard, when one belongs to
another release (the rule for the main database, cleat#3405); it verifies that the schema is not behind for the first shard
only.

Do not reorder the entries of a running deployment: the worker would start administering a different database.
`cleatctl --db` names one database; for anything that reads the plugin registry, give it the admin shard's
connection string.

**A sharded worker answers `501` to tenant provisioning, tenant drop, suspend and resume, and to a library
install, by design for now.** Provisioning, drop, suspend and resume go through the tenant provisioner, which
a sharded worker does not have: the rotating claim that serves many tenants is unavailable on it, so a tenant
created there would never run anything. A library install needs a store that can install, and a sharded store
cannot. Serving many tenants on a sharded worker is not part of 0.6.0; cleat#3606 tracks the whole limit (suspend
and resume, and with them the library install) and stays open for after it.

What does work across shards: a workflow definition is written to every shard, and a library bundle can be
published to every shard with `cleatctl library publish --replicate-to`
([Publishing to a sharded deployment](../how-to/deploy-workflows.md#publishing-to-a-sharded-deployment)).

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
