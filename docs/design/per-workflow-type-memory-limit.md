# A memory limit per workflow type, a per-tenant maximum, and an aggregate guard

**Status: design for the owner to decide. Nothing here is built.** The build is 0.7.0 (milestone 0.7.0, with the residency tuning in cleat#4187).
Written 2026-11-20 against develop `7d1d0e20` by WS-cleat-2; the line references were re-derived on develop `c29d6f0d` the same day, after cleat#4200 merged. Every code claim below was read or run on one of those trees; each carries the command that
re-derives it, because the code this describes moves.

## 1. What is being asked

cleat#4171 decided that one number is the default for every guest: **256 MiB** of linear memory per store, was 32 MiB. That is a ceiling, not a
reservation, and the owner asked for it uniform for 0.6.0. **The change itself is cleat#4200, which merged on 2026-11-20 at 05:19Z.** When this was written (develop `7d1d0e20`) the engine's
`DefaultMemoryLimitPages` was still 512 (32 MiB) and a per-language floor still raised C# to 64 MiB; develop now has 4096 and no floor (`git grep languageMemoryFloorBytes` finds nothing in `engine/wasmtime_options.go`), and the rest of this document assumes that.
This document designs what comes after a uniform number:

1. a memory limit **per workflow type** (a `workflow_defs` row: name and version), for the workflow that needs more than the default or less;
2. a **per-tenant maximum** on what a tenant's workflow types may request;
3. an **aggregate guard**, so many guests together cannot exhaust a worker (`--memory-soft-limit` is the existing one, see section 7).

## 2. Why one number stops being enough, and what is not known

What was measured (cleat#4195, section 9a of `docs/design/resident-suspended-runs.md`, a 10-CPU Linux VM, parked guests):

- **Go and Python: a parked guest's cost does not depend on the cap** (Python 8.99 MiB at a 32 MiB cap and 9.00 at 256, at N=16; Go 2.68 and 2.67).
- **C# does, after garbage:** a guest that allocated and dropped about 256 MiB costs 49.5 MiB at a 64 MiB cap and **65.7 MiB at 256**, about a third more.
  Whether a collection before parking recovers that is not measured (cleat#4187).
- Worst case for 64 resident runs: about 1.2 GiB of Go instances, 1.0 GiB of Python, **4.1 GiB of C# at a 256 MiB cap** (3.1 at 64).

**What is NOT known, and it is the main thing:** how much memory real workflows use. No metric reports a guest's linear memory (section 8: there is a per-definition estimate, which is not that). So every number in
this design that is not a measurement above is a choice, and the first deliverable is the means to measure.

## 3. What exists (verified 2026-11-20)

| Layer | Where | What it can do today |
|---|---|---|
| Operator default | `--wasm-memory-max-mb`, engine `DefaultMemoryLimitPages` (4096 = 256 MiB since cleat#4200; it was 512 = 32 MiB) | one number for every guest on the worker |
| Tenant | `tenant_settings` (`migrations/*/001_schema.sql`): `wasm_instance_timeout_ms`, `wasm_wall_clock_ceiling_ms`, `host_retry_budget_ms`, `max_workflow_duration_ms`; set by `cleatctl set-tenant-setting` | **tighten only**, durations only; **no memory column** |
| Run | `run_*` columns in `workflow_leases`, read by `RunLimitsReader.GetRunLimits` (`engine/run_limits.go`) | tighten below the tenant; durations only |
| Workflow definition | `workflow_defs` (name, version, tenant): `max_history_length`, `task_queue`, `exposure`, `input_validation_disabled`; set by `cleat deploy --max-history-length N` | **a per-type resource limit already exists** for history length, with 0 meaning "the global value" |

Re-derive: `grep -n 'wasm_instance_timeout_ms\|max_workflow_duration_ms' migrations/postgres/001_schema.sql`;
`git grep -n 'max-history-length' cmd/cleat/main.go`; `git grep -n 'func ClampToCeiling' engine/`.

How a per-run value reaches the guest today, because memory would take the same road: `Engine.perExecutionInstanceTimeout` composes run against tenant
and hands the raw value to `backend.PerExecution(timeout)` (`engine/executor.go:385` and `:897`); the **wasmtime backend clamps it against the operator's
flag inside `PerExecution`** (`engine/backend_wasmtime.go:310`, the clamp itself at `:319`) and `configureStore` applies the result. The memory limit is applied in the same
`configureStore` from `b.limits.memoryLimitBytes`, so a per-execution memory value would flow the same way. `PerExecution` has one implementation and
one interface method (`engine/backend.go:43`), and 27 test references.

**Two existing facts decide the shape.**

1. `ClampToCeiling` is `min(request, ceiling)` with an unset request (`<= 0`) resolving to the ceiling, over `time.Duration`. Its comment's warning is about
   a setting where a LARGER value is the safer one (a minimum, a floor on an interval), and **memory is not that**: it is smaller-is-safer, like every duration
   there, so the direction is not an obstacle (an earlier draft of this document said it was; that misread the warning). What does not fit is two things:
   (a) an unset request resolves to the ceiling, but here it must resolve to the DEFAULT, because the default and the ceiling become different numbers,
   which is the new thing (for every existing duration the operator's value is both); and (b) the type is `time.Duration`, and memory is bytes.

2. `max_history_length` shows the precedent for **where** a per-type value lives and **who** sets it: a deploy-time flag on `cleat deploy`, stored on
   the definition. It is not clamped. It bounds the workflow's own history, which costs the deployer, not other tenants; memory is a shared resource,
   which is why this one needs a ceiling above it.

## 4. Proposal

Three values, one resolution. Names are proposals.

- **Operator:** `--wasm-memory-max-mb` stays the **default** for a type that sets nothing. Add `--wasm-memory-ceiling-mb`: the most any request may
  reach. **Its default equals the default limit**, so raising is an opt-in the operator makes, and an upgraded worker changes nothing.
- **Tenant:** `tenant_settings.wasm_memory_max_mb`: the most this tenant's workflow types may request. NULL means the operator's default (a tenant is
  granted more than the default only by an explicit setting). It is capped at the operator ceiling.
- **Workflow type:** `workflow_defs.max_memory_mb`, set by `cleat deploy --max-memory-mb N` (0 = unset). A request.

```
tenantCap = min(tenant.wasm_memory_max_mb or operator.default, operator.ceiling)
effective = request == 0 ? operator.default : min(request, tenantCap)
```

- **Lowering is always allowed** (a type may ask for less than the default; it only constrains itself).
- **A request above `tenantCap` is refused at deploy**, loudly, naming the cap, and is **also clamped at run time**. Both are needed: the tenant's cap can
  be lowered after a definition was deployed, and the run-time clamp is the one that cannot be skipped.
- **No per-run tier.** Nobody has asked for one, and it can be added later without changing this. (`Engine.resolveLimit` says to read `ClampToCeiling`'s
  comment before adding a fourth tier or field, because "smaller is safer" is not general; checked here, and it holds for memory.)

The comparison is `min(request, cap)`, the same shape as `ClampToCeiling`, but it is written as its own function for the two reasons in section 3 (an unset request
resolves to the default and not to the cap; the type is bytes).

### How it reaches the guest

Extend the seam that exists: `PerExecution` gets the effective memory beside the timeout. **Do it through an optional interface**, as `RunLimitsReader`
did (its comment gives the reason: adding a method to the main interface edits every test double, and there are 27 test references to `PerExecution`).
`PerExecution` would copy `b.limits` as it does and set `memoryLimitBytes` from the resolved value, after clamping to the operator ceiling inside the
backend, where both numbers are in scope, which is the argument `PerExecution`'s own comment makes for the timeout.

## 5. A limit below what a runtime can start in

cleat#4200 deleted the per-language floor (C#: 64 MiB, `languageMemoryFloorBytes`), because a 256 MiB default can never be below it. A **per-type** limit can be: a .NET guest
does not start below 52 MiB (measured on a 704 KB hello-world, cleat#4077) and dies with `.NET runtime failed to initialize`, a message that names
neither the limit nor the cause. So a per-type request needs a **minimum check at deploy**: refuse `--max-memory-mb` below the language's start-up
minimum, saying why. That is a small table again, but a different thing from the floor: it **validates** a number the operator chose and never
silently raises one. **Only the C# minimum is measured.** Go, Python and TypeScript minima are not, and a table row is added only with a measurement.

## 6. Cost

Reach of a new `workflow_defs` column, using `max_history_length` as the measure: **16 files** (`git grep -l 'max_history_length\|MaxHistoryLength' -- '*.go' '*.sql' ':!*_test.go'`),
across three dialects' schema, deploy, library and copy paths. A `tenant_settings` column needs three dialect read paths, which the compiler finds
(`tenantSettingsFromMillis` takes one parameter per column for that reason); and `TenantSettings` is all `time.Duration`, so memory is new fields, not a
reuse of the "same three fields" argument in `engine/run_limits.go`. Plus `cleat deploy`, `cleatctl`, the admin API and the docs. This is the larger part of
the work; the engine change is small.

## 7. The aggregate guard

**Do not admit on the sum of limits.** 64 resident guests at a 256 MiB limit sum to 16 GiB, which is a number about no actual memory use (Go and Python parked
cost about 9 MiB or less, section 2). Admit on **measured resident cost**: a wasm linear memory never shrinks, so a parked instance's current memory size
is known exactly at the moment it parks, and the worker can sum the real figure.

- `--memory-soft-limit` (default 0.80) scales concurrency down from a fraction of memory (never below 1 until the hard limit, cleat#4498), and its source is `MemoryMonitor`: cgroup v2 `memory.current` less `inactive_file`, else host-wide
  `/proc/meminfo`, else the Go heap, **which cannot see wasmtime's memory** (macOS lands there). The first two sources do see guest memory (the second is host-wide, so it also counts everything else on
  the machine); **only the Go-heap fallback is blind** (cleat#4196).
  A guard that reads nothing is worse than none: an operator believes it is on.
- The residency admission (0.6.0, per the coordinator's order) is **measured per-language cost plus a count backstop**; this design's guard is the same
  thing with per-type numbers once those exist.

## 8. A prerequisite, and it comes first

**No metric reports a workflow's guest linear memory.** There are two things that are easy to mistake for one:

- **Worker-wide gauges**: `RecordMemoryRSS`, `RecordMemoryAvailable` and `RecordMemoryTotal` (`cmd/cleat-worker/setup.go:3323-3325`).
- **A per-(tenant, definition) estimate**: `cleat_workflow_memory_estimate_bytes` (registered in `monitoring/prometheus/metrics.go`, recorded on the same tick at
  `setup.go:3330`, persisted through `RecordWorkflowMemorySample` and `LoadMemoryEstimates` in three dialects). It is an EWMA (alpha 0.3,
  `cmd/cleat-worker/memory_controller.go`) of the **change in `MemoryMonitor.SampleUsage()` between the start and the end of each execution**
  (`setup.go:3629-3652`), and it exists to size concurrency.

The estimate is a useful rough guide to a typical footprint, and it is **not what a limit needs**, by reading (none of this was measured): (1) it is a mean, and a
limit is set from a peak; (2) the delta spans a whole execution on a worker that runs others concurrently, so another execution's allocation lands in this one's
sample, and a sample is dropped when the end reading is not above the start; (3) it measures whatever the monitor's source measures (cgroup memory, host-wide
available memory, or, on the Go-heap fallback, nothing a guest allocates), not the guest's linear memory; (4) it carries no language label.

The guest's linear-memory size is read in exactly one place outside tests, to place a scratch buffer (`engine/backend_wasmtime.go:1167`), and is never recorded.
The searches behind both statements, each run with its known positives (the three gauges and the estimate must all appear in the first):
`git grep -n -i 'RecordMemory\|MemoryEstimate\|memory_estimate' -- 'cmd/cleat-worker/*.go' 'monitoring/prometheus/*.go' ':!*_test.go'` and
`git grep -n '\.DataSize(' -- 'engine/*.go' 'cmd/cleat-worker/*.go' ':!*_test.go'`. (An earlier draft of this section carried a different search that did not match
the gauges it named, so it could not have found the estimate; that is how this one was missed.)

**Build first, independently:** record the guest's linear-memory size at the end of each execution (peak, since a wasm memory never shrinks), as a histogram
labelled by workflow name and language. It is small, helps today, and its first month of data is what decides every open question below.

## 9. Decisions for the owner

1. **Is raising above the default needed in 0.7.0?** If not, ship only lowering: it needs no ceiling flag and no split between default and ceiling (the operator's value stays both, as for every duration),
   and the split is the part of the design that is new. Raising is what costs design: a second operator number and a tenant grant. Recommendation: **lowering and the metric first; raising when
   a workflow actually needs it**, and the metric will say which.
2. **Who may set the tenant maximum?** Today only an operator can: the one write method, `PostgresStore.WriteTenantSettings`, has one non-test caller,
   `cleatctl set-tenant-setting` (registered for Postgres only, `cmd/cleatctl/ported.go`), and no route in `cmd/cleat-worker` or `cmd/cleat` matches. So the tenant maximum
   would be an operator **grant**. Whether it should ever be tenant-settable, and how a MySQL or SQL Server deployment would set it at all, is the decision.
3. **Operator ceiling default = the default limit** (raising is opt-in). Recommendation: yes.
4. **Refuse at deploy and clamp at run time**, both. Recommendation: yes.
5. **No per-run tier.** Recommendation: not now.

## 10. What this does not settle

- The per-language start-up minima other than C#, which have to be measured before the table in section 5 has rows.
- Whether collecting before a C# guest parks recovers the third measured in section 2 (cleat#4187). If it does, the case for a per-language limit
  is weaker.
- What real workflows use. This is a design for choosing a number, written without one.
- Whether `cleat_workflow_memory_estimate_bytes` is good enough as a first rough guide (section 8 argues it is not a basis for a limit, by reading; its values were not looked at).
- The component path (Python, C#) and the core path read the same `b.limits.memoryLimitBytes` in `configureStore`, by reading; a per-type limit
  has not been run through either.
