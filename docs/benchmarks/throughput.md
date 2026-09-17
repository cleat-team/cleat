# Cleat Engine Throughput Benchmarks

> Measured 2026-05-15. Scope narrowed 2026-09-17 to what was actually run.

## What this document is, and what it is not

**Read this before quoting any number below.**

Everything here was measured on **one laptop** — a Ryzen 5 5500U with
default-configured Docker PostgreSQL. That is a development machine, not a
representative deployment, and no figure here should be compared against a
number another project published from provisioned cloud hardware.

The two classes of result mean very different things:

| section | what it measures | what it does **not** measure |
|---|---|---|
| In-Process Benchmark Results | Go framework overhead — closure dispatch, mutexes, allocation | WASM instantiation, any database, any network |
| Database Benchmark Results | PostgreSQL operations against a local Docker instance | WASM, network, contention from real worker fleets, any other dialect |

In particular, the in-process figures in the millions of workflows per second
are **the cost of calling a Go function**. A workflow that does no durable call
and touches no database is not a workflow cleat would ever run. Those numbers
are a useful ceiling for framework overhead and are meaningless as a throughput
claim.

**Removed 2026-09-17, deliberately:** a "Throughput at Scale (Projected)"
section, an "Estimated throughput tiers" table, a cost-per-dollar calculator
built on top of those estimates, and a "Retention Verification" section whose
every file path (`internal/host/*`) no longer exists. None of it was measured;
the calculator converted the no-database microbenchmark into a dollars figure.
Publishing fewer honest numbers beats publishing many unreliable ones. What a
replacement needs is stated at the end.

## Methodology

### Hardware

| Component | Detail |
|-----------|--------|
| CPU | AMD Ryzen 5 5500U with Radeon Graphics (12 threads) |
| RAM | 18 GB (DDR4) |
| Disk | NVMe SSD (238 GB, /localssd) |
| OS | Linux 6.17.0-23-generic |

### Software

| Component | Version |
|-----------|---------|
| Go | 1.25.7 |
| PostgreSQL | 16.13 (Docker, Debian 16.13-1.pgdg13+1) |
| PostgreSQL config (Docker defaults) | shared_buffers=128MB, effective_cache_size=4GB, work_mem=4MB, max_connections=100, random_page_cost=4 |

### Benchmark types

Two classes of benchmark were run:

1. **In-process microbenchmarks** (`benchmarks/cleat_bench_test.go`): Measure pure framework overhead using in-process `HostCalls` (no WASM compilation). All operations are in-memory. Results represent the upper bound of workflow throughput.
2. **Database benchmarks** (`benchmarks/db_bench_test.go`, tag `db_bench`): Measure PostgreSQL-backed operations: claim queries, event history insert/select, heartbeats, compaction. These show real-world database throughput for the event history store.

Each benchmark ran with `-benchtime=10s` for stable results. Metrics reported by `testing.B`:
- **ns/op**: nanoseconds per workflow execution
- **wf/s**: workflows per second
- **steps/s**: durable steps (API calls) per second
- **B/op**: bytes allocated per operation
- **allocs/op**: allocations per operation

---

## In-Process Benchmark Results

These benchmarks use simulated HostCalls with no WASM or network overhead, providing a ceiling for framework throughput.

### Simple (Sequential) Workflow

Steps are sequential `DurableCall` invocations. Measures pure framework overhead.

| Steps | Iterations | ns/op | wf/s | steps/s | B/op | allocs/op |
|-------|-----------|-------|------|---------|------|-----------|
| 10    | 60,169,414 | 206.9 | 4,832,213 | 48,322,130 | 96 | 2 |
| 100   | 9,679,707 | 1,215 | 823,276 | 82,327,604 | 96 | 2 |
| 1000  | 1,000,000 | 11,541 | 86,645 | 86,645,087 | 96 | 2 |

### Fan-Out Workflow

Spawns N parallel child workflows, each with one `DurableCall`, then awaits all.

| Children | Iterations | ns/op | wf/s | steps/s | B/op | allocs/op |
|----------|-----------|-------|------|---------|------|-----------|
| 10       | 2,430,218 | 4,342 | 230,306 | 4,836,434 | 2,041 | 28 |
| 100      | 295,557 | 41,158 | 24,296 | 4,883,567 | 20,084 | 214 |
| 500      | 47,293 | 251,893 | 3,970 | 3,973,912 | 133,180 | 1,264 |

### Saga Workflow (Happy Path)

N saga steps with forward + compensation registered; all succeed, no compensation triggered.

| Steps | Iterations | ns/op | wf/s | steps/s | B/op | allocs/op |
|-------|-----------|-------|------|---------|------|-----------|
| 10    | 448,267 | 26,504 | 37,730 | 377,301 | 12,703 | 197 |
| 100   | 47,522 | 272,429 | 3,671 | 367,068 | 125,558 | 1,910 |
| 1000  | 4,460 | 2,605,525 | 383.8 | 383,800 | 1,243,731 | 20,503 |

### Saga with Compensation

Last step fails; all previous steps are compensated. N-1 forwards + N-1 compensates.

| Steps | Iterations | ns/op | wf/s | steps/s | B/op | allocs/op |
|-------|-----------|-------|------|---------|------|-----------|
| 10    | 235,983 | 54,235 | 18,438 | 331,889 | 25,120 | 399 |
| 100   | 20,768 | 504,205 | 1,983 | 392,697 | 244,678 | 3,822 |

### AI Agent Loop (LLM Simulation)

Simulates LLM chat + tool invocations per prompt iteration.

| Prompts | Tools/Prompt | Iterations | ns/op | wf/s | steps/s | B/op | allocs/op |
|---------|-------------|-----------|-------|------|---------|------|-----------|
| 1       | 5           | 7,104,844 | 1,788 | 559,221 | 3,355,328 | 384 | 8 |
| 5       | 3           | 2,808,584 | 4,213 | 237,370 | 4,747,396 | 1,056 | 22 |
| 10      | 2           | 1,767,385 | 6,336 | 157,825 | 4,734,745 | 1,536 | 32 |
| 50      | 1           | 607,807 | 19,817 | 50,461 | 5,046,106 | 4,898 | 102 |

### WASM Compilation & Payload

| Benchmark | Iterations | ns/op | B/op | allocs/op |
|-----------|-----------|-------|------|-----------|
| CompilationInstantiation | 790,047 | 16,534 | 8,880 | 36 |
| PayloadRoundTrip | 8,416,813 | 1,701 | 64 | 2 |

---

## Database Benchmark Results

### Claim Query Latency

`SELECT ... FOR UPDATE SKIP LOCKED` against 5,000 pre-loaded instances.

| Workers | Iterations | ns/op | B/op | allocs/op |
|---------|-----------|-------|------|-----------|
| 10      | 47,106 | 279,383 | 3,535 | 63 |
| 100     | 78,912 | 145,917 | 4,488 | 66 |
| 1000    | 93,326 | 142,704 | 4,023 | 67 |

### Event History INSERT Throughput

Batch INSERT into `event_history` table within a transaction.

| Batch Size | Iterations | ns/op | events/s | B/op | allocs/op |
|------------|-----------|-------|---------|------|-----------|
| 1          | 12,602 | 915,411 | 1,092 | 1,974 | 49 |
| 10         | 4,616 | 2,634,420 | 3,796 | 15,312 | 382 |
| 100        | 624 | 18,682,439 | 5,353 | 148,657 | 3,712 |

### Event History SELECT Throughput

**Not measured.** The benchmark did not run: `event_history` lives in the
`cleat_bench` schema and was not on the default `search_path`, so both cases
errored out.

A previous revision of this document published that as a results table with
`FAIL (schema resolution)` in the cells, alongside a note that the production
code paths work correctly. A failed run is not a result, and a table is where a
reader looks for one. `LoadEventHistory` and `LoadEventHistoryPaginated` are
covered by the test suite; their **throughput is simply unknown** and is stated
that way here rather than rendered as a row.

### Heartbeat UPDATE Throughput

| Iterations | ns/op | updates/s | B/op | allocs/op |
|------------|-------|-----------|------|-----------|
| 10,000 | 1,022,802 | 977.7 | 759 | 20 |

### Compaction

Loading event history for compaction operations at different history sizes. Measures paginated cursor-based load.

| Events | Iterations | ns/op | events/s | B/op | allocs/op |
|--------|-----------|-------|---------|------|-----------|
| 10,000 | 1,689 | 6,988,304 | 847.2 | 106,264 | 10,793 |
| 100,000 | 250 | 48,455,905 | 8,255 | 106,264 | 10,793 |

---

## Bottleneck Analysis

### In-Process Framework

- **CPU-bound**: The framework overhead per step is ~11.5ns (1000-step case), dominated by closure dispatch and mutex operations.
- **Memory**: ~96 B/op for simple workflows, scaling linearly with steps for saga patterns.
- **Linear scaling**: Framework throughput scales linearly with cores (12-thread CPU).

### Database

- **I/O-bound**: Event history write throughput is limited by PostgreSQL WAL write rate and disk IOPS.
- **Claim contention**: At 1000 concurrent workers, claim latency is ~143us (down from 279us at 10 workers, indicating better saturation).
- **Compaction**: Loading 100K events for compaction is the most I/O-intensive operation.

---

## What a replacement benchmark has to do

This document cannot currently support a public performance claim. To become one
that can:

1. **Provisioned hardware**, not a laptop, with the instance type stated.
2. **A tuned PostgreSQL**, or the configuration stated as deliberately default —
   `shared_buffers=128MB` is a Docker default, not a deployment.
3. **The SELECT benchmark fixed** so the `cleat_bench` schema resolves, and its
   numbers measured rather than omitted.
4. **WASM in the path** for at least one end-to-end case, so there is a figure
   that describes a workflow rather than a function call.
5. **Methodology stated in the terms Temporal and DBOS use**, so the numbers are
   comparable rather than merely present.

Until then, the honest summary is the one at the top: framework overhead is very
low, single-node PostgreSQL write throughput is the bottleneck, and no number
here describes a deployment.
