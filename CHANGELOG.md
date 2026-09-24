# Changelog

> **UPGRADE NOTES** — Breaking changes are called out at the top of each
> release section. Read them before upgrading between versions.

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### UPGRADE NOTES — breaking

- **`dd_config.api_key` and `pd_config.routing_key` move into tenant secrets; the plaintext
  columns are dropped.** (cleat#1992)

  datadog-export and pagerduty-alert used to store the Datadog API key and the PagerDuty
  routing key in plain SQL columns. They now go through the same `plugin.Secrets` envelope
  encryption every other tenant secret uses, under the names
  `datadogexport.api_key.<config-id>` and `pagerdutyalert.routing_key.<config-id>` (one secret
  per config, not one per tenant, since a tenant can have more than one config of each kind).
  Admin routes are unchanged (`POST`/`PUT .../configs` still take `api_key`/`routing_key` in the
  request body, and responses still redact it) — only where the value lives has changed.

  No migration procedure is needed: **0.3.0 requires a fresh database, with no upgrade path
  from v0.2.0** (cleat#2058, owner decision 3). A fresh database never has a plaintext
  `api_key`/`routing_key` row to move, so `datadog-export`'s v4 migration and `pagerduty-alert`'s
  v3 migration simply drop the columns (`plugin.Migration.Up`, plain SQL) with nothing to carry
  forward.

- **A `cleatctl quota set` that creates a new tenant-quota row now enforces it by default.**
  (cleat#2046)

  `enforce` used to default to `false` on a fresh row, whether written by `cleatctl` or by the
  column's own SQL default: a quota counted and reported but never refused a run. `quota set
  --tenant X --limit-count N --window-seconds N` with no `--enforce` flag now creates the row
  with `enforce=true`, and the `tenant_quota.enforce` column's SQL default changes from
  `FALSE`/`0` to `TRUE`/`1` on all three dialects (a new plugin migration,
  `plugins/tenantquota/migrations.go` v3).

  **Who is affected: an operator who runs `quota set` on a tenant/resource that has no row
  yet, without passing `--enforce`.** That command now enforces the limit it creates, where it
  previously only tracked it. Pass `--enforce=false` to keep the old soft-tracking behaviour for
  a new row. **Existing rows are untouched** — the migration changes only the column default,
  which SQL applies solely to a row that omits the value, and `cleatctl` always supplies
  `enforce` explicitly; an `UPDATE` (an existing row, `quota set` with no `--enforce`) keeps
  whatever the row already had. A tenant with no `tenant_quota` row still has no limit at all —
  there is no tenant-creation hook that writes one (`cleatctl --create-tenant` is Postgres-only,
  cleat#1114, and unrelated to this).

- **A worker no longer migrates the database when it starts; migration is a deploy step.**
  (cleat#2117)

  `cleat-worker --migrate-only` applies the core and plugin migrations and exits `0` (any
  failure is non-zero). It is idempotent, needs no master key, and is safe if two run at
  once. A normal start now **verifies** the schema and refuses to start, with the
  remediation in the message, if a migration this binary ships is not applied. **One-line
  migration:** run `cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL"` (with
  `--migrate-db` for a role that has DDL rights) before starting workers, or start a
  single-node or development worker with `--migrate-on-start`, which restores the old behaviour.

  **A schema ahead of the binary still starts**, with a warning naming both versions, so a
  rolling upgrade (which migrates to the new version while old workers are still running)
  does not wedge on the workers it is replacing. A schema *behind* is refused.

  Concurrent migrators are now safe on **every** dialect. Measured before this change, four
  concurrent runs against an empty database: PostgreSQL 4 of 4 succeeded; **MySQL 1 of 4 and
  SQL Server 1 of 4** (the rest failed with `Duplicate key name`, a deadlock, and "already an
  object named"). MySQL and SQL Server now queue on a named lock, as PostgreSQL always did.

  Shipped launch sites updated: the Helm chart (a pre-install/pre-upgrade hook Job, `migration.*`
  values), `k8s/migrate-job.yaml`, the `.deb` systemd unit (`ExecStartPre`),
  `docker-compose.cluster.yml` (a one-shot `migrate` service), and the three `cleat init`
  templates and `make` dev target (`--migrate-on-start`). Anything of your own that starts a
  worker against a fresh or older database needs one of the two.

- **A `TERMINATE` close-policy child is now recorded `status='terminated'`, not
  `status='failed'`.** (cleat#1978)

  When a closing parent's `TERMINATE`-policy child is closed by
  `enforceParentClosePolicy`, its row now gets `status='terminated'`,
  `error_op='parent_close'`, `error_code=NULL`, and an `error_msg` naming the
  **parent's actual outcome** — "parent workflow completed", "parent workflow
  failed", "parent workflow was dead-lettered", "parent workflow was
  terminated", "parent workflow was cancelled", or, for `ContinueAsNew`,
  "parent continued as new (run \<id\>)" (`parentOutcomeMessage`,
  `engine/store_lifecycle.go`).

  Previously it wrote `status='failed'` with a fixed `error_msg` of "parent
  workflow terminated" **regardless of why the parent actually closed** — a
  parent that completed successfully still left its TERMINATE children
  reading "terminated" in their error text.

  **Consequences for callers.**
  - A parent or grandparent awaiting such a child via `GetChildResult`/
    `AwaitChild` now sees `Error` prefixed `"[TERMINATED] "` (per cleat#1974's
    existing convention for a directly-terminated child) rather than the
    ordinary failure shape. The child still reports `Completed=true,
    Failed=true` either way, so nothing that only checks those two fields
    needs to change.
  - Any code, dashboard, or alert that filters `workflow_instances` on
    `status='failed'` and expected TERMINATE-policy children to be included
    must now also check `status='terminated'`.
  - `docs/reference/workflow-lifecycle.md`'s status table is updated to
    match.

- **`--max-quota-events` now defaults to 50,000 instead of unlimited.** (cleat#1829)

  A workflow run that writes more than 50,000 events is now **continued as
  new** at that point rather than growing without bound. This is a rollover,
  not a failure: the durable call is refused before dispatch, so no side effect
  happens, the guest's defers drain as they do for an explicit
  `ContinueAsNew`, and the executor records a `continue_as_new` suspension.

  Previously the default was `0` = unlimited, and `--retention-days` did not
  help — it sweeps *terminal* runs, and a runaway is not terminal, so one
  looping workflow could fill `event_history`.

  **If you have a legitimate run that exceeds 50,000 events**, set
  `--max-quota-events` higher, or `0` to restore the old unbounded behaviour.
  The number is a starting point rather than a measurement.

  `--max-quota-children`, `--max-quota-concurrency-keys` and
  `--max-quota-schedules` are **unchanged and still unlimited**, deliberately:
  exceeding those fails the workflow rather than rolling it over, so a default
  would break working deployments.

- **A start payload that omits a declared entry-point parameter is now refused,
  instead of binding the zero value.** (cleat#1065)

  Previously an absent `string` bound `""` and an absent `int` bound `0`, in Go
  and AssemblyScript. Python has always refused, and Rust refuses unless the
  field is `Option<T>` or carries `#[serde(default)]` — so the SDKs disagreed
  about the same payload.

  Zero-binding **cannot tell "sent zero" from "sent nothing"**, permanently, for
  every caller. The workflow runs, the result is plausible, and nothing records
  that the value is not the one that was sent. The information belongs to the
  caller and was destroyed at the boundary.

  **Declaring a parameter optional is how a workflow says absence is meaningful:**

  | SDK | spelling |
  |---|---|
  | Go | `*T` — binds `nil` when absent |
  | AssemblyScript | a declaration-site default — `note: string = "x"` |
  | Python | a Python-level default |
  | Rust | `Option<T>` or `#[serde(default)]` |

  **Consequences.**
  - **This is a compile-time change, not a runtime one.** The binding lives in
    code generated into the guest module, so an already-deployed `.wasm` keeps
    the old behaviour until it is rebuilt. There is no flag day: a workflow
    adopts the new contract when someone recompiles it.
  - **No ABI bump.** The wire protocol, function signatures and memory contract
    are unchanged; what changed is the acceptance rule inside generated guest
    code, which `CurrentABIVersion` does not describe.
  - **A single `string` parameter is unaffected.** It receives the whole payload
    rather than a value looked up by name, so "absent" does not apply to it.
    Python has no such fast path and still refuses; that divergence is recorded
    in `tests/conformance/entry_point_binding_cases.json`.
  - **A composite parameter already refused** on absence. This makes absence
    uniform across types rather than adding a rule for scalars.
  - **A STORED payload is bound by whichever guest is current when it fires,
    not by the one that was current when it was written.** Rebuilding a target
    workflow changes the contract every payload already persisted for it is
    judged against. It is not re-validated at write time and cannot be: the
    module's `cleat.metadata` section carries no entry-point parameter list, so
    the host has nothing to check an input against.

    **Three dispatch paths do this, not one.** A cron schedule is the one that
    surfaced it (cleat#1705); naming only that one would describe the exposure
    as narrower than it is.

    | plugin | stored input | written by |
    |---|---|---|
    | `scheduler` | `schedules.input` | whoever registered the schedule |
    | `jobqueue` | `task_queue.input` | whoever enqueued the job |
    | `eventtriggers` | `event_subscriptions.input_template`, merged with the event body | an operator, plus the publisher |

        grep -rln 'env.StartWorkflow' --include='*.go' plugins/ | grep -v _test.go

    `jobqueue` is the sharpest of the three: a row whose `input` is NULL is
    dispatched as `{}` (`plugins/jobqueue/background.go`), which refuses **every**
    declared parameter rather than one. `eventtriggers` is the most exposed,
    because the workflow author controls neither half of the payload — the
    template is an operator's and the event body is a publisher's.

    There is no "bind it the old way" mode, and the reason is structural rather
    than a decision deferred: the binding lives in generated guest code, so a
    payload has no contract version to pin to. Pinning one would mean carrying
    a declared-parameter list and a binding epoch through every SDK's metadata.
    **The migration is the same as for any caller** — send the parameter, or
    declare it optional.

    **Where it surfaces.** The schedule fires, the run is claimed, and the guest
    refuses it, once per occurrence for as long as the schedule exists. The
    refusal reaches `workflow_instances.error_msg` and the API's `error` field,
    so it is queryable — but the scheduler counts the firing as *started*,
    because it only reports failures from `StartWorkflow`, and nothing links a
    failed run back to the schedule that started it.

  - **The pre-landing measurement did not cover this, and the denominator is
    why.** It was quoted as "0 confirmed omissions in 112 literal starts",
    which is accurate and answers a narrower question than it appears to: it
    scanned literal **start** calls. A cron payload is not a start call — it is
    a `ScheduleCron` argument, or a `POST /api/schedules` body — so the whole
    population of stored inputs was outside the scan. Re-measured across
    `cleat-ports` after the fact (cleat#1705):

    | | count |
    |---|---|
    | literal starts scanned before landing | 112, **0** omissions |
    | `ScheduleCron` call sites | 1, and it **omits** a declared parameter |
    | `create_schedule` **with** an input | 5, all complete |
    | `create_schedule` **without** an input | 7 |

    The one `ScheduleCron` site is what broke. The 7 input-less schedules are
    latent rather than failing: their crons (`*/5 * * * *`, `0 7 * * *`, daily)
    do not fire inside a test run, so nothing exercises them.

    Parse that population rather than grepping it — three of those
    `create_schedule` calls carry `inp=` on a continuation line, and a
    line-oriented count reports them as input-less, which inflates the
    omission count in the alarming direction.

### Added

- **`cleatctl quota get|set|list`, the operator surface for `tenant-quota`.** (cleat#2046)

  `quota get --tenant X [--resource R]` reads a tenant's quota row(s); `quota set --tenant X
  --resource R --limit-count N --window-seconds N [--enforce=true|false]` creates or updates
  one; `quota list [--tenant X]` lists rows for one tenant (every dialect) or every tenant
  (Postgres and MySQL — refused on SQL Server, where `tenant_quota`'s row-level security policy
  has no cross-tenant bypass, so an unscoped connection would read back an empty table rather
  than an honest error). All three work on Postgres, MySQL and SQL Server.

  **Stale-write refusal, mirroring `set-tenant-setting`.** `quota set` reads the current row
  first; a concurrent writer's change since that read refuses the write (`409`-shaped, not a
  silent overwrite) rather than clobbering it, and says so with a re-read command.

### Changed

- **The rate limiter refuses a cluster-wide limit it cannot honour, instead of quietly giving you
  a per-process one.** (cleat#1581)

  `mode: "db"` with no database configured used to log a warning and fall back to `memory`. The
  worker started, and because the memory limiter is an in-process map, **every worker served the
  full configured rate** — a four-worker deployment enforced four times the limit it was told to.
  An unrecognised mode did the same thing more quietly: the middleware tests `mode == "db"` and
  treats everything else as memory, so `"DB"`, `"database"` and any other near-miss also selected
  per-process limiting.

  Both now return an error from the plugin's `Init`, naming the reason.

  **Who is affected: only deployments that are already not getting what they asked for.** The
  default is unchanged (`memory`), and a config that does not set `mode` behaves exactly as
  before. If a worker now refuses to start, it was silently enforcing the wrong limits before.
  The fix is to supply a database or to say `mode: "memory"` and mean it.

### Fixed

- **A worker with no `CLEAT_SECRET_MASTER_KEY` now refuses to start on PostgreSQL and SQL Server when the
  database holds secrets, as it always did on MySQL.** (cleat#2123)

  The startup check read `tenant_secrets` across all tenants, and that read cannot see the table there: on
  PostgreSQL it raised (and the caller treated the error as "cannot tell"), on SQL Server it returned 0. So a
  worker started without the key against a database full of secrets booted normally and failed on the first
  workflow that resolved one, from inside a plugin call, with an error that does not mention keys.

  The check now reads each tenant's rows under that tenant's own context, **suspended tenants included**, and
  runs after the migrations. **A read that fails now refuses to start** rather than passing: with no key and a
  table it cannot read, the worker cannot tell whether it would fail on its first plugin call.

  **Upgrade note.** A deployment that was silently running without the key while holding secrets will now
  refuse to start. That is the check working; set `CLEAT_SECRET_MASTER_KEY` to the key the secrets were sealed
  with.

- **Write-ahead call intent now works on a sharded deployment, and an operator can resolve an
  ambiguous call there.** (cleat#1778)

  `ShardedStore` implemented `WorkflowStore` and not the two call-intent interfaces, so on a
  sharded worker:

  - Declaring any operation in `--write-ahead-intent-ops` made that operation **fail outright**.
    The engine refuses rather than downgrading to at-least-once — deliberately, since a
    durability guarantee that is configured, believed and absent is the failure this feature
    exists to remove — so the call was not dispatched and the error named the store.
  - `ResolveStep`, the documented way out of an ambiguous call, answered
    `store *engine.ShardedStore cannot resolve call intents`. An operator holding an answer from
    the external service had no way to record it.

  All three intent methods now route to the shard that owns the workflow, like every other
  per-workflow operation. The pending row, its completion, the replay that reports it ambiguous
  and the operator's resolution all land in the same database.

  **Who is affected: sharded deployments that declared a write-ahead operation.** Unsharded
  deployments are unchanged — the three dialect stores always implemented these methods. A shard
  whose store cannot honour the guarantee still fails loudly, and now names the shard.

- **A sharded deployment now honours per-tenant and per-run limit overrides, and says so when it
  can't.** (cleat#1853)

  `ShardedStore` was also missing `GetTenantSettings` and `GetRunLimits`, which all three dialect
  stores implement. Both are reached by a type assertion that returns **with no log at all** on
  failure, so every workflow on a sharded deployment silently got the worker's flag values for
  `WasmInstanceTimeout`, `WasmWallClockCeiling`, `HostRetryBudget` and `MaxWorkflowDuration`,
  regardless of what a tenant or a run had configured.

  **This is not a limit escape.** A tenant's settings are already clamped to the operator's, and a
  run's to its tenant's, so the fallback can only ever be wider than intended, never past the
  operator's ceiling.

  Both now route to the shard that has the answer. `GetRunLimits` routes by workflow ID, like the
  rest of `ShardedStore`. `GetTenantSettings` has no workflow ID to route by — every shard was
  opened for the same tenant — so it tries each shard and uses the first non-empty result, which
  survives an operator having written the override to only one shard (the writer, `cleatctl
  set-tenant-setting`, takes one connection and has no fan-out across shards). A shard that
  genuinely cannot answer now logs a warning naming itself, once per request, instead of nothing.

### Added

- **The audit log is now tamper-evident: each tenant's rows form a SHA-256 hash chain, and `cleatctl audit verify` checks it.** (cleat#2047)

  Every `audit_events` row carries `seq`, `prev_hash` and `row_hash`, and each tenant has a head
  row in `audit_chain_heads`. An edited row, a row removed from the middle, and rows removed from
  the end are each reported, by kind, at the first place they occur. `cleatctl audit verify
  (--tenant <id> | --all-tenants) [--json]` exits `0` clean, `1` on a break, and `2` when it could
  not look, and the two non-zero values are different on purpose.

  **What it is not:** the chain proves the integrity of what was recorded, not that everything was
  recorded, and it does not stop a database administrator who rewrites a whole chain and its head
  together. `docs/reference/audit-log.md` states both, with the encoding an offline verifier needs.

  Behaviour changes to know about:
  - **Existing rows are not backfilled.** They stay unchained and are outside the guarantee.
  - **Retention is per tenant.** It deletes an expired prefix of a tenant's chain (at most 5,000 rows
    per tenant per hourly sweep) and records a floor, instead of one cross-tenant `DELETE`. A backlog
    of expired rows now drains over several sweeps.
  - **Each request is a short transaction that locks its tenant's head row,** so a tenant's appends
    serialise. Different tenants do not contend.
  - **Text that cannot be stored (invalid UTF-8, NUL) is replaced with U+FFFD instead of dropping the
    whole event.**
  - **A value too long for its column is truncated with a `...[truncated]` marker instead of failing the
    insert** (`path` 700 characters and 800 UTF-16 units, `method` 255, the free-text columns 4,096). On
    MySQL and SQL Server an over-long path used to leave no audit row at all.
  - **The MySQL `audit_events.timestamp` column becomes `DATETIME(6)` holding UTC** (it was
    `TIMESTAMP(6)`, which stops at 2038).
  - **`cleatctl audit verify --retention-days N`** also reports a retention floor that covers rows too
    young to have expired.

- **Tenant-secrets master-key rotation: a key ring and `cleatctl reseal-secrets`.** (cleat#1991)

  A key is now named by an integer version. `CLEAT_SECRET_MASTER_KEY_VERSION` (default `1`, which is what every
  existing row carries), `CLEAT_SECRET_MASTER_KEY_PREVIOUS` and `CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION` let a
  worker open rows sealed under the key being retired while sealing new ones under the new key. `cleatctl
  reseal-secrets [--dry-run]` re-seals every row online, verifying before it writes and writing conditionally so a
  concurrent `set-secret` is not undone. A worker that cannot open some stored version refuses to start and names it.
  See `docs/how-to/use-secrets.md`.

  **The system now checks it.** Every worker publishes the key versions it can open in `admin.workers`, and a
  write at version *v* (`set-secret`, and each row `reseal-secrets` moves) is refused while any live worker
  cannot open *v*, naming the worker. The worker's start (publish its keys, read every stored secret) and a
  write (read the registry, write the row) are serialised by one named database lock, so a worker cannot start
  while a write is landing that it would then be unable to read. A worker whose membership loop stalled longer
  than its stale window re-registers and re-checks, and stops if it now holds a secret it cannot open.

  **Upgrade notes.**
  - **Every worker now registers in `admin.workers`**, not only those started with
    `--cluster-connection-budget`. The connection share still counts only workers that have a budget, so a
    mixed fleet divides what it did before.
  - **Complete the upgrade before the first rotation.** A worker from before this release is invisible to the
    gate unless it registered under the connection budget, and a registry row with no key set is read as
    "opens version 1 only".
  - A worker stalled for more than five minutes while still serving is invisible to a writer; see
    `docs/how-to/use-secrets.md`.

- **`audit-log` now records who: `user_id` on every row was the empty string, always.** (cleat#1881)

  The identity was already available — `oauth-provider` resolves an OAuth session to an email and
  puts it in request context — but nothing read it. A new neutral `auth.SubjectFromContext`
  (populated by `oauth-provider`, readable by any plugin) closes the gap without one plugin
  importing the other; an unauthenticated request still records an empty `user_id` and is never
  refused over a missing identity.

  **The ordering this depends on is now declared, not inherited from the alphabet.** `audit-log`
  reads context after the request has passed through every plugin wrapped around it, so it only
  sees `oauth-provider`'s value because `"oauth-provider"` happens to sort after `"audit-log"` in
  the tie-break `plugin.Discover()` falls back to when neither plugin declares a relationship.
  Renaming either plugin would have silently reverted `user_id` to always-empty with no test
  failing. A new plugin-contract clause (C14) and guard pin the real registered order instead —
  `plugin.PluginInfo.Requires` was considered and rejected for this, since it makes plugin
  discovery fail outright if the required plugin isn't registered, which is the wrong coupling
  between two independently-optional features.

- **`cleat build --target rust` refuses a workflow that iterates a `HashMap` or `HashSet` (R008).**
  (cleat#1864)

  Iterating a map is idiomatic Rust requiring no unusual act, unlike every other rule this checker
  enforces (opening a file, spawning a thread, reading the clock) — so it was the rule an author was
  least likely to suspect was missing. `HashMap`/`HashSet` default to a per-process random hash
  seed, so their enumeration order is not guaranteed by the language.

  **This is hardening, not a fix for an observed divergence.** cleat intercepts the WASI
  `random_get` import a `HashMap`'s hasher seeds from and binds it to a value deterministic in
  (workflow ID, step), so two replays of one workflow see the same order today regardless. The rule
  guards a language guarantee cleat is not relying on staying true, and its message does not claim
  otherwise.

  Construction, insertion, lookup and removal on a `HashMap`/`HashSet` are unaffected — only
  enumerating one (`for x in m`, `.iter()`, `.keys()`, `.values()`, `.into_iter()`, `.drain()`)
  triggers R008, which suggests `BTreeMap`/`BTreeSet`.

  **Detected via syntactic binding tracking, not a full type checker**, matching this checker's
  existing no-toolchain design: a function parameter's declared type or a `let`'s type
  annotation/constructor call is tracked within that function, and an enumerating use of a tracked
  name is reported. Two shapes are known, documented limits rather than silent misses — a map
  reached through a struct field, and one enumerated inline off a `.collect::<HashMap<_, _>>()`
  chain with no intermediate binding — see `testdata/vet-checks/rust/known_limit_*`.

- **Pre-emptive cancellation, with a terminal status of its own.**
  `POST /api/workflows/:id/cancel` accepts `{"preemptive": true}`, which stops the workflow and
  records **`cancelled`** rather than asking it to stop. (cleat#1153)

  **Why it exists.** Cancellation was cooperative *and unobservable*: `RequestCancellation` set a
  flag and left both stopping and reporting to the workflow, and `cancelled` was an error code
  rather than a status the engine ever wrote. So a run that honoured a cancellation and one that
  simply finished were **both `done`**, and an operator could not answer "did this stop because I
  asked it to?".

  **It runs the defers it owes.** A workflow with registered `defer` bodies goes to `terminating`
  carrying `cancelled` as its recorded outcome, is re-claimed, replays its history as a defer
  segment, and only then becomes `cancelled`. That is the same two-phase transition `terminate`
  uses, shared rather than rebuilt.

  **Asynchronous, like terminate**, and for the same reason (`tiers.yaml` decision D6). The
  endpoint answers `{"status": "cancelled"}`, which names the *outcome*; a caller that needs to
  know the run has finished polls the status.

  **Not a breaking change.** `preemptive` defaults to `false`: a client sending `{"reason": "..."}`
  gets the cooperative path and the `cancellation_requested` response exactly as before.

  **`cancelled` is a new terminal status**, so a client that switches exhaustively on workflow
  status should add a case. It is excluded from the active-child count, from parent close
  policies, and from every other "is this run settled?" predicate — see
  `docs/reference/workflow-lifecycle.md`.

- **`completed_by` on the workflow object** — the worker that performed the terminal write.
  `assigned_to` is a *lease*, not an audit field: every terminal write clears it while fencing on
  it, so it is blank on every finished run and cannot answer "which worker ran this" after the
  fact. Measured on a live database, `assigned_to` was blank on 185 of 185 terminal runs.
  (cleat#1118)

  **Schema change**, applied by `postgres/074`, `mysql/064` and `mssql/068`: `workflow_instances`
  gains a nullable `completed_by`. `postgres/075` additionally re-emits `finalize_workflow_status`,
  whose `done` and `failed` branches record it; the `ready` branch deliberately does not, because
  that run goes back on the queue and recording there would name whoever yielded.

  Returned by both read paths — `GET /api/workflows/:id` and the workflow listing.

  **Nothing is backfilled.** The column is NULL on every row written before the migration, and on
  any run that reached a terminal state without ever being claimed — no worker ran it. A run
  terminated through its *defer phase* names the worker that ran the defer phase rather than the
  workflow body: that transition clears the lease deliberately, to fence the old owner out.

### Changed

- **An update name is now reusable.** `POST /api/workflows/:id/update/:name` may be called any
  number of times over a run's life; each request is a row of its own with its own `promise_id`.
  Previously a name was consumed for the life of the workflow and the second request was refused
  with `409 update_name_used`. That `detail` value no longer occurs — `update_already_pending` is
  the only remaining 409 on this endpoint, and it clears when the in-flight request is answered.
  A client that branches on `detail` keeps working. (cleat#1416)

  **Schema change**, applied by `postgres/068`, `mysql/062` and `mssql/066`:
  `workflow_update_requests` gains a `request_id` column and is keyed
  `(workflow_id, request_id)` instead of `(workflow_id, update_name)`. Existing rows are backfilled
  from `update_name`, which is unique per workflow under the old key, so a workflow suspended
  mid-update across the upgrade completes against the correct row.

  Updates are **not** idempotent: a caller that retries after its first request was answered gets a
  new update rather than a replay. Carry your own key in the payload if you need at-most-once.


### UPGRADE NOTES — breaking

- **Terminating a workflow that has registered `defer` bodies is now asynchronous,
  and runs those bodies before the workflow becomes terminal.** Migrations
  `postgres/040`, `mssql/043` (MySQL needs none).

  Previously `TerminateWorkflow` wrote `status = 'terminated'` and then released
  the workflow's sticky assignment and concurrency keys. The registered defers
  never ran — and the resources a defer body would have released were dropped
  anyway, by the host, in the wrong order, with nothing recording that anything
  was owed.

  Now such a workflow goes to a new non-terminal status, **`terminating`**,
  carrying the outcome it will be given. A worker claims it like any other
  workflow, replays its history as a *defer segment* — the body does not run
  again; it is refused any new work — runs the outstanding defer bodies, and only
  then applies the recorded outcome and releases the resources.

  **Consequences.**
  - A caller that terminates and immediately reads `status` may see `terminating`
    rather than `terminated`, and must poll. This is `tiers.yaml`'s decision D6.
  - A workflow with **no** registered defers still terminates in one step, as
    before. Most deployments will see no change at all.
  - Terminating a workflow that is already in its defer phase terminates it
    immediately, cutting the cleanup short.
  - A defer phase never changes the outcome: if it traps, times out, or cannot
    start, the recorded outcome is applied anyway and the lost cleanup is logged.
    `defer_phase_deadline` (5 minutes) bounds it.
  - **Apply the migrations.** `postgres/040` widens `admin.claim_workflows` and
    the claim's partial indexes; `mssql/043` widens the filtered ones. A
    deployment running this code against the older schema keeps working — the
    cross-tenant claim falls back with a warning naming the migration — but its
    defer phases are never claimed, so every terminate waits out its deadline and
    skips the cleanup.

  **A closing parent's `TERMINATE` children work the same way**, and the change
  matters more there because it is a bulk operation: one closing parent used to
  drop every child's concurrency keys and sticky assignment at once, before any
  of their defers had run. A child that owes cleanup now goes to `terminating`
  carrying `pending_terminal_status = 'failed'` — the close policy's own
  outcome, not `terminated` — and is failed once its defers have run. A child
  that owes none is failed immediately, as before.

  The admin force-resolve verbs are unchanged: they still terminate in one step
  and still skip their defers.

  See IMPROVEMENT-PLAN §3.75, §3.112 and §3.114, and
  `docs/reference/workflow-lifecycle.md` for the whole state machine.

- **Workflow definition names are now per-tenant.** `workflow_defs`' primary key
  becomes `(tenant_id, name, version)`, and the three foreign keys that reference
  it — from `workflow_instances`, `workflow_tags` and `workflow_routing` — carry
  `tenant_id` too. Migrations `postgres/035`, `mysql/034`, `mssql/038`.

  Two tenants can now each hold their own `order-processor`. Previously the name
  was a shared namespace: the second tenant to deploy one was refused, and before
  that (pre-0.2.0) it silently overwrote the first.

  **Consequences.** A deploy no longer returns `409` for a name another tenant
  holds — there is no conflict to report. `ErrWorkflowDefOwnedByAnotherTenant` is
  removed, along with the default-tenant adoption window that let a definition
  deployed before per-tenant ownership stay reachable by every tenant; on
  PostgreSQL that also removes `OR tenant_id = '00000000-...'` from
  `tenant_isolation_defs`, bringing it into line with SQL Server, which never had
  it. A workflow started for a tenant that has not deployed the definition it
  names is refused by the foreign key.

  **MySQL only:** `workflow_defs.tenant_id` was nullable with no default, unlike
  the other two dialects. `mysql/034` backfills `NULL`s to the default tenant and
  makes the column `NOT NULL DEFAULT`, as a primary-key column must be.

  See IMPROVEMENT-PLAN §3.77 and D7 in `tiers.yaml`.

- **Cross-schema child workflows are removed.** The `cleat_child_workflow_in_schema`
  host call, the `cleat:host-calls/durable-extended-children` component interface,
  the `--peer-schemas` worker flag and the corresponding surface in the Go, Rust,
  Java, Python and AssemblyScript SDKs are all gone. A worker started with
  `--peer-schemas` now fails on an unknown flag.

  It let a workflow start a child by writing a row directly into another
  PostgreSQL schema. That makes the other deployment's schema part of your API and
  its migrations part of your compatibility surface, and it had no settled answer
  for whose tenant the child belonged to — the definition lookup in the peer schema
  carried no tenant predicate, and where the target tenant could not be recovered
  from the schema name the insert ran with no tenant context at all.

  **Use the other pool's API instead**, the same way any two services talk. Nothing
  in `tiers.yaml` claimed this feature at any tier and no end-to-end test exercised
  it. See IMPROVEMENT-PLAN §3.78.

  ABI host-function count goes 59 → 58 on both backends. `CurrentABIVersion` is
  unchanged: nothing that remains changed shape.

- **Terminating a parent now closes its children.** `TerminateWorkflow` never
  called `enforceParentClosePolicy`, on any dialect, so terminating a parent
  left its `TERMINATE`-policy children running and its `REQUEST_CANCEL`
  children unflagged — while force-completing or force-failing the *same*
  parent closed them. Measured 2026-09-02: a child of a parent closed with
  `TerminateWorkflow` stayed `ready`; the same child under `AdminForceComplete`
  went to `failed`.

  Who this breaks: a deployment that relied on `terminate` being the narrow
  "stop this one workflow" verb. Its children now close with it —
  `TERMINATE` children are failed with `parent workflow terminated`, and
  `REQUEST_CANCEL` children have cancellation requested. `ABANDON` children are
  unaffected, as they always were.

  The design document says this is what should happen (*"`enforceParentClosePolicy`
  runs on parent terminal transition"*, *"children are cancelled with their
  parent, preventing orphan workflows"*), and the deciding argument is internal
  consistency: `adminForceResolve` is an operator verb on an unclaimed workflow
  setting a terminal status with a direct `UPDATE` — the same shape as
  `TerminateWorkflow` in every respect — and it enforced the policy. See
  IMPROVEMENT-PLAN §3.79.

### Added

- **`allowed_signals` can be set.** `GET` and `PUT
  /api/workflows/{id}/allowed-signals` read and replace the list
  `--require-signal-auth` checks a caller against, backed by
  `WorkflowStore.SetAllowedSignalCallers` on all three dialects. Until now
  nothing in cleat could write that column, so 0.2.0's note below — that the
  flag denied every signal with no supported remedy — described a gap that is
  now closed.

  `PUT` replaces the whole list; send it without a caller to revoke, or `[]` to
  clear. Both verbs are scoped to the calling tenant, and a workflow belonging
  to another tenant answers `404` rather than `403`, so the endpoint cannot be
  used to find out which ids exist.

  **`--require-signal-auth` still defaults to `false`.** Every workflow starts
  with an empty list and nothing sets one at start time, so enabling the flag
  denies every signal until callers are granted per workflow. Grant first, then
  enable. See `docs/reference/worker-config.md`.

- **An operator can resolve a call left ambiguous by a crash.** `POST
  /api/admin/instances/{id}/steps/{step}/resolve`, with `X-Confirm:
  resolve-step` and `{"response": "..."}`, records an outcome for a durable
  call that was in flight when the process died.

  Such a call leaves a pending row, and replay reports it as `[AMBIGUOUS]` and
  says to check the external service before retrying — with nowhere to put the
  answer. An `AmbiguityResolver` could supply one, but the resolve path returns
  immediately when none is configured, which is most deployments, so the
  workflow reported the same ambiguity on every replay forever.

  The response is written to the event as though the call had returned it,
  because that is what replay has to see. What keeps it honest is the new
  `resolved_by` field, written to the same row in the same statement: the
  outcome is usable by replay and permanently marked as **asserted by an
  operator rather than observed**. The row must still be pending, so an
  operator racing a worker cannot overwrite a real result — whoever writes
  first wins. IMPROVEMENT-PLAN §1.4 phase F.

- **An operator can re-replay a stopped workflow.** `POST
  /api/admin/instances/{id}/re-replay`, with `X-Confirm: re-replay` and
  `{"generation": N}`, returns a `failed`, `terminated` or `dead_lettered`
  workflow to `ready`: the claim, heartbeat and error fields are cleared and
  the generation is bumped, so a stale worker's late write cannot land. History
  is **kept** — the workflow resumes from what it recorded rather than starting
  over. The non-terminal statuses are refused because the dispatcher owns them.

  Re-replay refuses a workflow whose history holds an unresolved ambiguous
  call, and names the step: replaying one stops again in the same place for the
  same reason, so it points at the resolve endpoint above instead. This was the
  last of the three admin operations that was a stub returning
  `not implemented` on all three dialects. IMPROVEMENT-PLAN §3.20.

- **`GET /api/workflows/{id}/history` reports `err_code` and `resolved_by` per
  event.** `err_code` is how the caller classified a failed call — the
  `ErrorCode` a `ServiceCaller` supplied through `CleatError` — recorded at the
  time of the failure, where history previously collapsed the whole
  classification to a single retryable-or-not bit. Its vocabulary is the one
  `workflow_instances.error_code` already stores, so one operator query matches
  in both tables.

  It is for reading, not for deciding: `err_non_retryable` stays authoritative
  for retry behaviour, deliberately. The two can legitimately disagree, because
  the guest's own retry policy travels across the ABI — and deriving
  retryability from the class instead would let an upgrade change the retry
  behaviour of workflows already in flight, which is the determinism bug §2.35
  exists to prevent. Both fields survive history compaction.
  IMPROVEMENT-PLAN §2.35.

### Fixed

- **Closing a workflow left concurrency slots and sticky-worker assignments
  held until their TTL.** `releaseWorkflowResources` runs the two best-effort
  cleanups that follow every commit taking a workflow out of the runnable set.
  Two paths committed such a transition without it:

  - `MySQLStore.TerminateWorkflow` execed its `UPDATE` and returned, where
    PostgreSQL and SQL Server both released. One slot per terminated workflow,
    on a tier-1 dialect (IMPROVEMENT-PLAN §3.76).
  - `enforceParentClosePolicy`'s TERMINATE arm failed every child of a closing
    parent and released nothing, **on all three dialects** — so one closing
    parent stranded a slot per child (IMPROVEMENT-PLAN §3.80).

  Bounded rather than leaked: `concurrency_keys.expires_at` is `NOT NULL` and
  the reaper deletes expired rows, so the slots freed themselves at the key's
  TTL. They freed themselves silently, with every workflow queued on those keys
  waiting out the window for nothing.

- **The reaper's reclaim-timeout default was too tight to safely cover a single
  failed-then-retried heartbeat, and an idle worker had no way to detect a
  database stall at all.** After a database stall, every running instance's
  `heartbeat_at` ages past the stale threshold at once; whichever worker's
  reaper reaches the database first after recovery could reclaim a run that is
  still alive, including its own. `engine.DBPinger` gives an otherwise-idle
  worker (nothing in flight, so no heartbeat write to prove liveness with) a
  real liveness signal, and `reapingIsSafe()` now requires both a recent
  confirmed contact-OK and an elapsed grace period since the last recorded
  trouble before trusting a stale `heartbeat_at` as evidence of a dead holder.

  **`--reclaim-timeout`'s derived default changes from `2*heartbeat` (floored
  at 10s) to `heartbeat + 3*dbCallDeadline(heartbeat) + heartbeatRetryInterval(heartbeat)
  + reclaimSlack` (floored at 10s) — about 14.5s at the default 5s
  `--heartbeat`, up from 10s.** This is a wider safety margin, not a
  behaviour anyone has to opt into: the old value undercounted a single
  heartbeat call that fails and is retried. A real network-level stall (not
  just a slow-but-reachable server) also keeps a failing call blocked until
  the stall itself clears rather than until its own client-side deadline —
  measured directly against a real PostgreSQL container under `docker
  pause` — so the invariant also accounts for the wait before that retry is
  even issued, which at `--heartbeat` below one second gets no faster a
  retry than the worker's own ordinary cadence. Finally, the modeled worst
  case has zero margin at `--heartbeat` >= 4s (the two formulas are
  algebraically identical there), and does not account for the retry
  needing a fresh connection — real and documented on SQL Server, which
  marks a connection bad after a cancelled call whose own cancel-drain also
  fails, exactly what a genuine stall produces — so `reclaimSlack` (a fixed
  1s) covers what the model leaves out rather than widening a term that
  means something else. An explicit `--reclaim-timeout` is unaffected.

  MySQL's `ReapStaleInstances` also now runs inside an explicit transaction —
  under `interpolateParams=true`, the previous autocommit statement could keep
  committing server-side after the caller's context was cancelled, so a
  caller-visible "deadline exceeded" did not mean the reclaim had not
  happened. Postgres and SQL Server were already transactional here.
  cleat#2005.

- **The reaper can no longer be fooled into reclaiming a live run by a
  whole-fleet database stall, only by a genuinely dead worker.** #2166
  covers a worker that itself observed database trouble; this covers the
  complementary gap — a fleet-wide stall silences heartbeat *writes* while
  reads keep working, so every running row ages past the reclaim threshold
  together, and whichever worker's reaper reaches the database first after
  recovery reclaims runs that are still alive, including its own, with no
  trouble ever recorded on its own side.

  A new optional `DBStallDetector` capability (implemented on all three
  dialect stores) reports the shape of the currently-stale set: how many
  running rows have missed at least one heartbeat, how many distinct
  workers they belong to, and whether not even one running row anywhere in
  scope has a heartbeat newer than the detection threshold. The reaper
  suppresses reclaiming for one tick whenever no recent heartbeat has
  landed anywhere, across more than one worker — a single fresh survivor
  anywhere blocks suspicion outright — and keeps suppressing until either
  the stale set genuinely clears or the full reclaim window elapses a
  second time, at which point it reclaims anyway and logs once: a
  persistent stall-shaped set is by then more likely a genuine mass
  failure than a database outage. A sharded deployment evaluates and
  suppresses each shard independently, so one stalled shard cannot pause
  reclaiming on a healthy sibling. If the shape probe itself fails, that
  shard's reclaim is skipped for the tick rather than proceeding
  unsuppressed — a slow or failing read over the very table about to be
  updated is itself stall-shaped, so "could not check" fails closed.

  Detection deliberately uses a **shorter** threshold than reclaim
  eligibility itself: gating suspicion on the same window `reclaimAfter()`
  reclaims at would miss a fleet stall lasting somewhat less than that
  window, because individual rows cross it staggered rather than all at
  once, and each gets reclaimed the instant it does — precisely the harm
  this exists to prevent. **Full protection holds for stalls up to about
  23s at the default `--heartbeat` (detection latency plus the reclaim
  window), degrading to none by about 33s** (one more reaper tick, the
  worst case for when the stall is first observed) — see
  `stallProtectionLower`/`stallProtectionUpper` in `cmd/cleat-worker`.
  Suppression is sticky once an episode opens: a tick where one worker's
  heartbeat lands first — un-suspecting the shape while its siblings are
  still individually stale — keeps suppressing on the same episode clock
  rather than releasing the laggards on that survivor's heartbeat alone.
  This protection is per-episode, not per-row: a reaper that never
  observed the stall's opening tick has no episode to be sticky about, and
  can still reclaim a laggard within about one heartbeat retry interval
  plus reconnect time of one worker's heartbeat landing while its
  siblings' have not — the gap between one worker's recovery and the rest
  is not itself modeled here.

  **Worst case, a genuinely dead worker's run now takes up to about 39s to
  reclaim at the default `--heartbeat`** (twice the ~14.5s reclaim window
  plus one reaper tick), up from that window alone, if its discovery
  happens to coincide with an unrelated fleet-wide stall being suppressed.
  New metric `cleat_suspected_db_stall_total`, labeled by shard, counts
  every tick this suppression fires. cleat#2006.

- **`ReapStaleInstances` truncated its reclaim timeout to whole seconds on
  all three dialects, halving #2166's 1s `reclaimSlack` at the default
  ~14.5s `--reclaim-timeout`** (PG: `"%d seconds"` over
  `int(timeout.Seconds())`; MySQL: `INTERVAL ? SECOND` over `int(...)`;
  MSSQL: `DATEADD(SECOND, ...)` over `int(...)`). At the default, a row
  actually became reclaimable at 14s rather than 14.5s; at `--heartbeat
  2.9s` (R=10.9s), only 0.1s of the documented slack remained. #2180 fixed
  the same truncation in `StaleSetShape` only, so the stall detector's
  `Stale` count (millisecond-precise) and the reap statement it feeds
  (second-truncated) could disagree about which rows were reclaimable, up
  to just under a second apart. Now millisecond-precise (microsecond on
  MySQL) on all three, matching `StaleSetShape`. cleat#2189.

## [0.2.0] - 2026-08-10

### UPGRADE NOTES — breaking

- **SQL Server 2022 is now the minimum.** `migrations/mssql/011` uses
  `ISJSON(payload, VALUE)`, whose second argument requires 2022, so that the
  payload columns accept the JSON scalars PostgreSQL and MySQL have always
  accepted — without it, `DeliverSignal` and `CreateUpdateRequest` failed on
  any SQL Server built from the shipped schema. `README.md` and
  `docs/reference/database-backends.md` previously said 2017+; nothing has ever
  tested below 2022.
- **`--require-signal-auth` now defaults to `false`.** It gates a check that
  reads `workflow_instances.allowed_signals`, and nothing in cleat can write
  that column — no store method, no API endpoint, no CLI verb, no SDK call. The
  check denies when the list is empty, so with the flag on by default every
  cross-workflow signal, every plugin-originated signal and every external HTTP
  signal was denied, and the documented remedy (add `"*"` to `allowed_signals`)
  could not be carried out. A deployment that wants the old behaviour can pass
  `--require-signal-auth=true`, but should know that it denies every signal.
  The default goes back to `true` when there is a way to populate the list.

- **A deploy no longer overwrites a workflow definition owned by another
  tenant; it fails instead.** `workflow_defs` is keyed by `(name, version)`
  with no tenant in the key, and all three backends upserted on that key — so
  the second tenant to deploy a given name silently replaced the first
  tenant's WASM bytes, and the first tenant's workflows then executed the
  second tenant's code. `DeployWorkflowDef` now records the deploying tenant
  and refuses to write over a definition that belongs to someone else,
  returning an error wrapping `engine.ErrWorkflowDefOwnedByAnotherTenant`.

  Who this breaks: a multi-tenant deployment in which two tenants deploy the
  same definition name. That previously "worked" in the sense that one row
  survived and served both; it now fails for whichever tenant does not own the
  name. If you were relying on one shared definition across tenants, deploy it
  as the default tenant (`00000000-0000-0000-0000-000000000000`) and do not
  redeploy it as a specific tenant — a definition owned by the default tenant
  stays readable by every tenant, which is what this table's PostgreSQL RLS
  policy has always allowed.

  What upgrades cleanly: every definition in an existing database is owned by
  the default tenant, because `PostgresStore` hardcoded that value and
  `MSSQLStore`'s `MERGE` omitted the column. Such a definition is *adopted* by
  the first tenant that redeploys it, so ordinary redeploys keep working and
  ownership takes effect from then on. Until a definition has been redeployed
  once, a tenant other than its creator can still take it over.

  This does not make two tenants able to hold the same name — that needs the
  tenant in the primary key, and with it three foreign keys per dialect. The
  name remains a global namespace; squatting one is now loud instead of
  silent. IMPROVEMENT-PLAN §3.12.

- **Workers now refuse to start on a PostgreSQL connection that bypasses
  row-level security.** Every tenant-scoped table has RLS enabled and FORCEd,
  and for `GetWorkflowByID` and `ListWorkflows` those policies are the only
  tenant isolation there is — neither carries an application-level `tenant_id`
  filter. PostgreSQL never applies RLS to a superuser, so a superuser
  connection returned every tenant's data from those calls. Every
  configuration previously shipped connected as one.

  With `--require-auth` (default true), a worker whose `--db` role is a
  superuser, has `BYPASSRLS`, or owns the tables without `FORCE` will now log
  the reason and exit rather than serve traffic it cannot isolate.

  To upgrade:

  1. Apply `migrations/postgres/005_app_role.sql`, which creates the
     `cleat_app` role and grants it what the engine needs — no ownership, no
     DDL.
  2. Give it a password: `ALTER ROLE cleat_app LOGIN PASSWORD '...';` The
     migration deliberately does not, so no credential lives in the
     repository. (`docker-compose.cluster.yml` does this from
     `CLEAT_APP_PASSWORD` via `deploy/postgres/900-app-role.sh`, but
     `docker-entrypoint-initdb.d` only runs on a *first* initialisation, so an
     existing deployment must run it by hand.)
  3. Point `--db` at `cleat_app`, and pass the previous owner DSN as
     `--migrate-db` — migrations need DDL rights that `cleat_app` does not
     have. `--migrate-db` defaults to `--db`.

  `--rls-check=off` restores the old behaviour for a single-tenant deployment
  that does not want this. `--rls-check=require` refuses regardless of
  `--require-auth`.

- **The root `schema.sql` has been deleted.** `migrations/postgres/` is the
  only schema source. The deleted file was a second, hand-maintained copy that
  had drifted into a strict subset: no `finalize_workflow_status` (which the
  engine calls on every workflow completion, with no fallback), no RLS
  policies, and no `admin.tenants`. A database built from it could not complete
  a workflow. Apply every file in `migrations/postgres/` in lexical order.

- **`docker-compose.cluster.yml` mount layout changed.** `migrations/postgres`
  is now mounted read-only at `/opt/cleat/migrations` and applied by
  `deploy/postgres/100-apply-migrations.sh`; `deploy/postgres` is
  `/docker-entrypoint-initdb.d`. Deployments that copied the old volume block
  must update it.

- **`DURABLE_TEST_DB` is renamed to `CLEAT_TEST_DB`.** The old name still works
  and warns.

- **`engine/testutil.TestDB` now fails instead of skipping** when a DSN is
  configured for its dialect but cannot be reached. Asking for a database and
  not getting one is a broken configuration, not an absent one — this is how a
  CI job stayed green for months without ever connecting. With no DSN
  configured it still skips.

### Added
- Engine test coverage for WASM backends (wasmtime, wazero) exceeding 40%
- Integration tests for MySQL and MSSQL store backends
- Unit tests for SignalWorkflow, DurableScheduleInvoke, and RegisterUpdateHandler
- WasmDiskCache and in-memory WASM cache unit tests
- CGO dispatch layer unit tests for component_cgo.go
- Engine streaming, deferral, dispatch, and callbacks unit tests
- Mock-DB coverage for WorkflowLoader DB methods
- Multi-backend coverage for MySQL and MSSQL store methods
- Concurrency key and shard error-path tests for ShardedStore
- WASM lock, memory, and scan unit tests
- Plugin loader, migration, events, credentials, and encryption tests
- Engine/app.go comprehensive test suite (22 functions, 100% coverage)
- QueryBuilder and Dialect SQL helper unit tests
- Regression tests for critical-path PostgresStore methods

### Changed
- Project renamed from "durable" to "cleat" across the codebase

### Removed
- TinyGo support for compiling Go workflows to WASM. TinyGo is an
  embedded-systems toolchain and lacked the standard library coverage this
  project needs. The standard Go toolchain targeting `wasip1` (`--target go`)
  is now the only supported way to compile Go workflows to WASM.

### Fixed
- Multi-database CI failures in MySQL and MSSQL integration tests
- Tenant isolation: restore tenant_id filter on GetWorkflowByID
- JSONB handling in PostgreSQL store
- All 54 MySQL integration tests now pass
- MSSQL test container configuration to avoid MCR pull block
- Error message quality improvements for CLI, worker, and engine
- Race condition fixes for concurrent workflow execution
- Wasmtime memory buffer and signal handling fixes
- Plugin test failures in manifest and eventtriggers
- Auth middleware and tenant store test repairs
- Project root detection in engine tests
- ABIVersion type comparison in mssql_store_test.go
- Expand fake SQL driver coverage for admin-prefixed queries
- Corrected CompleteWorkflow status assertion from "completed" to "done"
- MySQL test schema TEXT to VARCHAR for assigned_to column
- **`CreateUpdateRequest` rejected ordinary payloads.** A non-JSON update
  payload failed outright on MySQL and SQL Server, and one containing a quote
  or a backslash failed on all three — `workflow_update_requests.payload` never
  received the JSON encoding signals got in the same fix. Both the encode and
  the decode are now shared, so every dialect stores and returns what the
  caller passed in.
- **Workflows could not complete on SQL Server.** `json.Marshal` of a nil map
  returns `null`, not nil, so a workflow with no query handlers wrote the JSON
  value `null` into `query_state` — which PostgreSQL and MySQL accept and the
  shipped SQL Server schema rejects with
  `CHECK (ISJSON(query_state) = 1)`. `CompleteWorkflow`, `FailWorkflow` and
  `ContinueAsNew` all failed there. On the other two dialects the query state
  was silently stored as `null` rather than `{}`.
- **`CreateSchedule` could not create a schedule on SQL Server.**
  `json.RawMessage` binds as `VARBINARY`, so `workflow_schedules.input` was
  written as the binary rendering of the JSON, and the shipped schema's
  `CHECK (ISJSON(input) = 1)` rejected the row. Every scheduled workflow on a
  SQL Server built from `migrations/mssql/001_schema.sql` failed to be
  created. The test schema declared no such constraint, which is why the suite
  never showed it.
- **No `cleat-worker` could start against MySQL either.** The migration runner
  split each file on every `;`, including semicolons inside comments and inside
  stored-procedure bodies, so neither shipped MySQL file could be applied:
  `001_schema.sql` failed with `Error 1064` on a semicolon in a comment, and
  `003_procedures.sql` — which creates `finalize_workflow_status`, the
  procedure the engine calls on every workflow completion with no fallback —
  was cut into fragments and its `DELIMITER` directive sent to the server. A
  worker pointed at a MySQL database whose schema had not been built by hand
  logged the error and exited. Statement splitting is now comment-, string- and
  `DELIMITER`-aware, and `multi-db-ci.yml` runs the migration tests against
  live MySQL and SQL Server.
- No `cleat-worker` could start against PostgreSQL: a session-scoped
  `SET search_path` in the migration files broke the migration runner's own
  bookkeeping, and concurrent workers raced each other's DDL at boot. Both
  migration runners now hold an advisory lock, and the core runner
  schema-qualifies its tracking table.
- The shipped schema created its objects in a schema named after the
  connecting role rather than `public`, because `POSTGRES_USER=cleat` collides
  with a schema `001_schema.sql` creates.
- `ContinueAsNew` had never worked on PostgreSQL (an INSERT listed nine columns
  and supplied eight values).
- `AssignedTo` was overwritten with an empty string on every claim.
- `tenant_id` was not written by `CreatePromise`, `DeliverSignal` or
  `CreateSchedule`, so those rows were invisible to the tenant that created
  them.
- `PollSignal` deleted the signals it read, contradicting its own contract and
  both other backends. It is on the live signaller path.
- The auto-generated startup API key was never created on any PostgreSQL
  deployment: the count query named `tenant_api_keys` unqualified while the
  table is `admin.tenant_api_keys`. With `--require-auth` defaulting to true, a
  fresh cluster had no key and no way in.
- `cleat build` chose its output `.wasm` filename at random, because entry
  points were read from a map in iteration order.
- The `kvstore` and `feature-flags` plugins did not work on MySQL or SQL
  Server: unquoted `key` (a reserved word in both), no `LIMIT` on SQL Server,
  and JSON columns that could be neither read nor written there.

## [0.1.0] - 2026-05-13

### Added
- Durable execution engine with deterministic replay model
- Multi-database support: PostgreSQL, MySQL, SQL Server (MSSQL)
- WASM compilation pipeline (Go to wasip1, TinyGo support)
- 22 built-in plugins (blobstore, event-triggers, feature-flags, kafka-connect,
  llm, notifications, pagerduty-alert, slack-notify, webhook-ingest, and more)
- 5 language SDKs: Go, Rust, Python, Java, AssemblyScript
- Svelte 5 admin dashboard with embedded web UI
- CLI toolchain: build, vet, deploy, versions, schedule
- Prometheus metrics endpoint and OpenTelemetry tracing support
- wazero WASM runtime (pure Go, no CGo required)
- PostgreSQL-backed work queue, timer service, and blob store
- Deterministic replay from event history (Temporal-style model)
- Saga pattern with compensating transactions
- Durable promises for cross-workflow coordination
- Virtual Object (entity workflow) pattern for stateful actors
- Continue-as-new for workflow history compaction
- Heartbeat support for long-running operations
- Server-side retry with configurable backoff policy
- Signal patterns (fire-and-forget, request-response, polling)
- Update handler pattern (bi-directional RPC with validation)
- Query handlers for read-only workflow state inspection
- Sharded store for horizontal scalability
- Tenant isolation with schema-per-tenant support
- Workflow versioning with minimum version support

[Unreleased]: https://github.com/cleat-team/cleat/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/cleat-team/cleat/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cleat-team/cleat/releases/tag/v0.1.0
