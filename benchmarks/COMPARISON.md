# Benchmark Comparison: Cleat vs Temporal vs DBOS

Standardized methodology and results for comparing durable workflow engine
performance. These benchmarks measure framework overhead for common durable
execution patterns, not application-level performance.

> **Status**: Template ready for results. Run `benchmarks/comparative/runner.sh`
> on a dedicated machine to populate this document. See "How to Reproduce"
> below for exact commands.

---

## Methodology

### Hardware Configuration

Results are only comparable when all three frameworks run on the same hardware.
Document the following for each benchmark run:

| Parameter | Value (placeholder) |
|-----------|---------------------|
| CPU model | TBD |
| Cores / threads | TBD |
| Base / turbo frequency | TBD |
| Turbo Boost | Disabled during benchmarks |
| RAM capacity / type / speed | TBD |
| Disk model / type / filesystem | TBD |
| OS / kernel | TBD |
| CPU governor | `performance` |
| Process isolation | `taskset` to dedicated cores |

### Software Configuration

| Parameter | Cleat | Temporal | DBOS |
|-----------|-------|----------|------|
| Version / commit | TBD | TBD | TBD |
| Go version | TBD | TBD | N/A |
| Node version | N/A | N/A | TBD |
| PostgreSQL version | TBD | TBD | TBD |
| PostgreSQL config | TBD | TBD | TBD |
| Temporal server mode | N/A | TBD | N/A |
| Database backend | PostgreSQL | TBD (SQLite/Postgres) | PostgreSQL |

### System Configuration

Each framework is benchmarked with the following baseline configuration:

- **PostgreSQL 16**: same instance shared by Cleat and DBOS (Temporal uses its
  own backend). PostgreSQL `shared_buffers` = 25% of host RAM, `work_mem` = 64MB,
  `max_connections` = 100.
- **Cleat worker**: `--concurrency 10`, `--heartbeat 5s`, `--poll 500ms`.
- **Temporal dev server**: started separately per benchmark run.
- **DBOS**: `@dbos-inc/dbos-sdk`, running against the shared PostgreSQL instance.

### Rules of Engagement

1. **Same hardware** -- All frameworks on the same machine or cloud instance type.
2. **Same PostgreSQL** -- Shared instance for all frameworks that support it.
3. **Three runs, report median** -- Each workload is run three times; median
   values are reported. Individual run values are included to show variance.
4. **Warm-up** -- 10-second warm-up phase before measurement begins. Results
   during warm-up are discarded.
5. **Measurement window** -- 60-second measurement window after warm-up.
6. **Isolation** -- CPU frequency scaling disabled (`cpupower frequency-set
   --governor performance`), Turbo Boost disabled, no other load on the machine.
7. **10% significance threshold** -- Differences under 10% (ratio 0.91-1.10) are
   reported as "not significant".

---

## Results Table

### Primary metrics

| Workload | Config | Metric | Cleat | Temporal | DBOS | Cleat vs Temporal | Cleat vs DBOS |
|----------|--------|--------|-------|----------|------|-------------------|---------------|
| Sequential | steps=10 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Sequential | steps=10 | p50 (ms) | TBD | TBD | TBD | TBD | TBD |
| Sequential | steps=10 | p99 (ms) | TBD | TBD | TBD | TBD | TBD |
| Sequential | steps=100 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Sequential | steps=1000 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Fan-out | children=10 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Fan-out | children=100 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Fan-out | children=500 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Saga (happy) | steps=10 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Saga (happy) | steps=100 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Saga (happy) | steps=1000 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Saga (failure) | steps=10 | steps/s | TBD | TBD | TBD | TBD | TBD |
| Saga (failure) | steps=100 | steps/s | TBD | TBD | TBD | TBD | TBD |
| LLM agent | 1x5 | steps/s | TBD | TBD | TBD | TBD | TBD |
| LLM agent | 5x3 | steps/s | TBD | TBD | TBD | TBD | TBD |
| LLM agent | 10x2 | steps/s | TBD | TBD | TBD | TBD | TBD |
| LLM agent | 50x1 | steps/s | TBD | TBD | TBD | TBD | TBD |

### Secondary metrics

| Workload | Config | Metric | Cleat | Temporal | DBOS |
|----------|--------|--------|-------|----------|------|
| Sequential | steps=1000 | Memory RSS (MB) | TBD | TBD | TBD |
| Sequential | steps=1000 | DB CPU (%) | TBD | TBD | TBD |
| Sequential | steps=1000 | Startup time (ms) | TBD | TBD | TBD |
| Fan-out | children=500 | Memory RSS (MB) | TBD | TBD | TBD |
| Saga (happy) | steps=1000 | Memory RSS (MB) | TBD | TBD | TBD |

---

## Benchmark Workloads

### 1. Single Activity (Simple)

**Purpose**: Measure pure framework overhead per durable call.

A single workflow that performs N sequential `DurableCall` operations against a
no-op service. Each call returns immediately. This isolates the framework's
per-call overhead (event recording, replay cache, history persistence) without
application I/O.

**Parameters**: steps = 10, 100, 1000.

**What it measures**: Framework overhead per durable call.

### 2. Sequential Chain

**Purpose**: Measure workflow with data-dependent branching between steps.

A workflow that chains N operations sequentially, where each call's output
determines the next call's input. Models real-world pipelines like
order-processing flows. Tested in the "Simple" and "Sequential" workloads.

**Parameters**: steps = 10, 100, 1000.

**What it measures**: Sequential execution overhead with data-passing.

### 3. Parallel Fan-Out

**Purpose**: Measure child workflow management overhead.

A parent workflow spawns N child workflows, each performing a single step.
The parent waits for all children to complete. Models fan-out patterns like
sending notifications to multiple recipients.

**Parameters**: children = 10, 100, 500.

**What it measures**: Child workflow creation, execution, and result-gathering
overhead. Parallelism efficiency and context switching costs dominate.

### 4. Saga with Compensation

**Purpose**: Measure compensation scaffold overhead.

A workflow executes N forward steps. Each step has a registered compensating
action. Two modes:

- **Happy path** (all steps succeed): Measures the cost of maintaining the
  compensation chain and executing all steps.
- **Failure path** (last step fails): Measures compensation execution overhead
  as all N-1 compensations run in reverse order.

**Parameters**: steps = 10, 100, 1000 (happy); 10, 100 (failure).

**What it measures**: Compensation scaffolding overhead and reverse-execution
cost.

### 5. Timer/Sleep Heavy

**Purpose**: Measure timer management overhead.

A workflow performs N `DurableSleep` calls of varying durations. Workers must
suspend the workflow, persist the wake-up time, and resume after sleep expires.
This exercises the timer subsystem and the suspend/resume cycle.

**Parameters**: calls = 10, 100 (with durations from 1ms to 10s).

**What it measures**: Timer registration, suspension, and wake-up overhead.

---

## How to Reproduce

### Prerequisites

```bash
# Required for all frameworks
sudo cpupower frequency-set --governor performance
echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo

# Cleat
go install ./cmd/cleat
go install ./cmd/cleat-worker

# Temporal (optional)
curl -sL https://temporal.download/cli.sh | sh
temporal server start-dev --db-file /tmp/temporal-bench.db &

# DBOS (optional)
npm install -g @dbos-inc/dbos-sdk
```

### Run All Benchmarks

```bash
# From repository root, run the unified runner:
./benchmarks/comparative/runner.sh

# This auto-detects available frameworks and runs each workload.
# Results are written to:
#   benchmarks/comparative/results/results-YYYY-MM-DD-HHMMSS.md
#   benchmarks/comparative/results/results-YYYY-MM-DD-HHMMSS.csv
```

### Run Single Framework

```bash
# Cleat only
./benchmarks/comparative/runner.sh --cleat-only

# Temporal only
./benchmarks/comparative/runner.sh --temporal-only

# DBOS only
./benchmarks/comparative/runner.sh --dbos-only
```

### Run Individual Workloads

```bash
# Cleat (via Go benchmarks)
go test -bench="." -benchtime=60s -benchmem -count=1 ./benchmarks/ 2>&1

# Temporal (per-workload)
cd benchmarks/comparative/workflows/01-sequential/temporal
go run main.go -warmup=10s -benchtime=60s

# DBOS (per-workload)
cd benchmarks/comparative/workflows/01-sequential/dbos
npx ts-node main.ts --warmup 10000 --benchtime 60000
```

### Custom Parameters

```bash
# Override defaults
./benchmarks/comparative/runner.sh --warmup 30s --benchtime 120s --concurrency 20
```

### Output Format

Results are reported in the `BENCHMARK_RESULT` format:

```
BENCHMARK_RESULT  name=SimpleWorkflow  config=steps=10  count=59320  elapsed=10.0s  wf_per_sec=48577  steps_per_sec=485766
```

The runner parses this format and generates comparison tables automatically.

---

## Caveats

### What These Benchmarks Measure

- **Framework overhead only**. All `DurableCall` targets are no-op stubs that
  return immediately. Real-world workflows will spend most of their time in
  application/service code, making framework overhead less significant.
- **Clean-room conditions**. Benchmarks run on an isolated machine with no other
  load. Real deployments share resources with other services.
- **Warm cache**. The warm-up phase ensures database query plans, connection
  pools, and JIT compilation are stable. Cold-start performance will differ.

### What They Don't Measure

- **End-to-end latency** through external services (HTTP calls, database
  queries). Stubs return in microseconds, not milliseconds.
- **Network overhead** in distributed deployments. All processes run on
  localhost (or the same machine).
- **Long-running workflow behavior**. Workflows run for seconds, not days or
  months. History compaction, large event histories, and long replay times
  are not exercised.
- **Failure and recovery scenarios**. No worker crashes, database failover, or
  network partitions are simulated.
- **Multi-tenant contention**. All workflows run in a single namespace/tenant.

### Known Limitations by Framework

**Cleat**:
- WASM compilation adds startup latency (not measured in per-call benchmarks).
- PostgreSQL is the only backend; no comparison against Temporal's SQLite mode.
- In-process `go test -bench` execution does not use the full
  production worker path (no `SELECT ... FOR UPDATE SKIP LOCKED` claim loop;
  the test harness calls the engine directly).

**Temporal**:
- Dev server performance differs from production Temporal Cloud.
- SQLite backend (dev server default) is used unless PostgreSQL is configured.
- Temporal's default retry and timeout settings are used; tuning may change
  results.

**DBOS**:
- TypeScript runtime adds JIT warm-up overhead.
- DBOS workflows run as HTTP handlers; benchmark includes HTTP round-trip.
- Node.js garbage collection pauses affect tail latency (p99).

### Configuration Differences

- **Concurrency model**: Cleat uses goroutine-per-workflow with a configurable
  concurrency limit. Temporal uses a slot-based polling system. DBOS uses
  Node.js event loop with async/await. Each model has different overhead
  characteristics.
- **Persistence batching**: Cleat and DBOS batch writes to PostgreSQL. Temporal
  (dev server) uses SQLite with different batching behavior.
- **History storage format**: Cleat stores event history as ordered rows in a
  single table. Temporal uses a more complex history sharding scheme. DBOS
  records function invocations as database transactions.

### Interpreting Results

- A Cleat advantage in simple workloads likely reflects the lower overhead of
  the in-process test harness versus a full server-based framework.
- A Temporal advantage in fan-out workloads likely reflects Temporal's mature
  child workflow implementation versus Cleat's simpler approach.
- A DBOS advantage in saga workloads likely reflects DBOS's transactional model
  versus the replay-based approach used by Cleat and Temporal.
- Real workload performance depends primarily on the service call latency.
  Framework overhead is the dominant factor only for very fast operations (<1ms).

### Reporting Checklist

- [ ] CPU model and frequency governor documented
- [ ] Turbo Boost disabled
- [ ] RAM type and speed documented
- [ ] Disk model and filesystem documented
- [ ] OS and kernel version documented
- [ ] Go version documented
- [ ] Node.js version documented
- [ ] PostgreSQL version and config documented
- [ ] Temporal server mode documented
- [ ] Three runs completed for each workload
- [ ] Median values reported
- [ ] Warm-up phase verified (10s)
- [ ] Measurement window verified (60s)
- [ ] All five workload patterns tested
- [ ] Variance across runs documented
