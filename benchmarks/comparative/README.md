# Comparative Benchmarking: Cleat vs Temporal vs DBOS

Standardized methodology for comparing durable workflow engine performance
across four benchmark patterns. These benchmarks measure framework overhead
for common durable execution patterns, not application-level performance.

## Hardware Requirements and Documentation

When reporting results, document the following hardware and software
configuration:

### Required hardware documentation

- **CPU**: model name, core count, logical thread count, base/turbo frequency.
  Note whether Turbo Boost / Turbo Core is enabled or disabled.
- **RAM**: total capacity, type (DDR4, DDR5, etc.), and speed (MHz).
- **Disk**: model, type (NVMe vs SATA SSD vs HDD), filesystem (ext4, XFS, etc.),
  and mount options.
- **Network**: loopback only (all benchmarks run locally). Note if any network
  filesystem or remote database is involved.

### Required software documentation

- **OS**: distribution, kernel version (`uname -a`).
- **Go**: version (`go version`). Required for Cleat and Temporal.
- **Node.js**: version (`node --version`). Required for DBOS.
- **PostgreSQL**: version, configuration (`shared_buffers`, `max_connections`,
  `work_mem`, `effective_cache_size`). Required for DBOS and optional for
  Temporal (if using SQLite vs PostgreSQL).
- **Temporal**: server version, database backend.
- **Cleat**: commit hash or version.

### Recommended hardware

- Dedicated machine or cloud instance with no other load.
- At least 4 CPU cores, 8 GB RAM.
- SSD storage for the database.
- Linux preferred for CPU frequency control and cgroup isolation.

## Rules of Engagement

Follow these rules exactly to ensure reproducible, comparable results.

### 1. Same hardware

All three frameworks MUST be benchmarked on the **same physical machine**
(or same cloud instance type). Do not compare results across different
hardware configurations.

### 2. Same PostgreSQL (when applicable)

When DBOS benchmarks use PostgreSQL, Temporal and Cleat should also use the
**same PostgreSQL instance** for their persistence layer (if they support it).
If Temporal uses its default SQLite backend, document this separately.

### 3. Three runs, report median

Each benchmark workload is run **three times** on each framework. Report the
**median** value across the three runs. Also report individual values to show
variance.

### 4. Warm-up

Each benchmark must include a **10-second warm-up** phase before measurement
begins. During warm-up, the framework processes workflows but results are
discarded. This allows JIT compilation, connection pooling, and database query
plan caching to stabilize.

### 5. Measurement window

Each benchmark uses a **60-second measurement window** after warm-up.
Metrics are collected only during this window.

### 6. Isolation

- Disable CPU frequency scaling during benchmarks:
  ```bash
  sudo cpupower frequency-set --governor performance
  echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
  ```
- Pin benchmark processes to dedicated cores using `taskset` if running on a
  multi-core machine.
- Close all other applications and services.
- Run one framework at a time.

## Metrics Collected

### Primary metrics

| Metric         | Unit       | How collected                   | Meaning                                      |
|----------------|------------|---------------------------------|----------------------------------------------|
| Throughput     | steps/s    | Framework benchmark output      | Durable API calls (steps) per second         |
| Workflow rate  | wf/s       | Framework benchmark output      | Workflow completions per second              |
| P50 latency    | ms         | Per-workflow timing distribution| Median single-workflow latency               |
| P99 latency    | ms         | Per-workflow timing distribution| 99th percentile single-workflow latency      |

### Secondary metrics

| Metric       | Unit  | How collected                                  | Meaning                          |
|--------------|-------|------------------------------------------------|----------------------------------|
| DB CPU       | %     | `pg_stat_activity` or `top` during run         | Database process CPU utilization |
| Memory RSS   | MB    | `/usr/bin/time -v` or `ps -o rss`              | Benchmark process resident set   |
| Allocations  | B/op  | Go `-benchmem` flag (Cleat, Temporal)          | Allocations per workflow         |
| Alloc count  | allocs/op | Go `-benchmem` flag (Cleat, Temporal)     | Allocation count per workflow    |

## How to Run Each Framework

### Cleat

```bash
# From repository root
go test -bench="." -benchtime=10s -count=1 ./benchmarks/ 2>&1   # warm-up (discard)
go test -bench="." -benchtime=60s -benchmem -count=1 ./benchmarks/ 2>&1 | tee cleat-results.txt
```

Or use the unified runner:
```bash
./benchmarks/comparative/runner.sh
```

### Temporal

Prerequisites:
- Temporal Go SDK v1.25+ (`go.temporal.io/sdk`)
- Temporal server (dev server or production)

1. Start the Temporal dev server:
   ```bash
   temporal server start-dev --db-file /tmp/temporal-bench.db &
   sleep 3
   ```

2. Run each workload:
   ```bash
   cd benchmarks/comparative/workflows/01-sequential/temporal
   go run main.go -warmup=10s -benchtime=60s

   cd ../../02-fanout/temporal
   go run main.go -warmup=10s -benchtime=60s
   ```

3. Or use the unified runner with `--temporal` flag.

Temporal main.go programs connect to `localhost:7233` by default. Use
`-address` flag to override.

### DBOS

Prerequisites:
- DBOS SDK (`@dbos-inc/dbos-sdk`)
- Node.js 18+
- PostgreSQL running locally
- DBOS configuration file (`dbos-config.yaml`)

1. Initialize DBOS in the workload directory:
   ```bash
   cd benchmarks/comparative/workflows/01-sequential/dbos
   npx dbos init
   npx dbos migrate
   ```

2. Run the benchmark:
   ```bash
   npx ts-node main.ts
   ```

3. Or use the unified runner with `--dbos` flag.

**Important**: Each DBOS workload directory needs its own `dbos-config.yaml`
and `package.json`. See the DBOS documentation for setup instructions.

## Workload Descriptions

| Pattern    | Workload                    | What it measures                                        |
|------------|-----------------------------|---------------------------------------------------------|
| Simple     | N sequential steps          | Pure framework overhead per step                        |
| Fan-out    | N child workflows           | Child workflow creation, execution, and result gathering|
| Saga       | N steps with compensation   | Step registration, execution, and reverse compensation  |
| LLM agent  | N prompts x M tool calls    | Iteration loop with two call types per iteration        |

### Parameter values tested

| Workload              | Parameter values                     |
|-----------------------|--------------------------------------|
| Simple sequential     | Steps: 10, 100, 1000                 |
| Fan-out children      | Children: 10, 100, 500               |
| Saga compensation     | Steps: 10, 100, 1000 (happy path)    |
| Saga with failure     | Steps: 10, 100 (fail at last step)   |
| LLM agent loop        | Prompts x Tools: 1x5, 5x3, 10x2, 50x1|

## Results Interpretation Guide

### Reading the comparison table

The unified runner generates a markdown table like this:

```
| Workload              | Config  | Cleat steps/s | Temporal steps/s | DBOS steps/s | Cleat vs Temporal | Cleat vs DBOS |
|-----------------------|---------|---------------|------------------|--------------|-------------------|---------------|
| Simple                | steps=10| 485766        | 125000           | 98000        | 3.88x faster      | 4.96x faster  |
```

The "Cleat vs X" column shows the ratio. A value > 1.0 means Cleat is faster;
< 1.0 means the other framework is faster.

### Significance threshold

A difference of **10% or less** (ratio between 0.91 and 1.10) is considered
within noise and marked as "~" (no significant difference).

### What the benchmarks tell you

- **Simple workflow**: Measures raw framework overhead per durable call.
  Frameworks with lower call overhead will show higher steps/s.
- **Fan-out workflow**: Measures child workflow management overhead.
  Parallelism efficiency and context switching costs dominate.
- **Saga workflow**: Measures compensation scaffolding overhead.
  The cost of maintaining compensation chains and executing reverse steps.
- **LLM agent workflow**: Measures iteration overhead with mixed call types.
  Realistic for AI agent workloads with tool-use patterns.

### Sources of variance

Common sources of benchmark variance:

1. **CPU frequency scaling**: Can cause 20-50% variance. Disable with
   `cpupower frequency-set --governor performance`.
2. **Background processes**: Cron jobs, log rotation, monitoring agents.
3. **Database vacuum/checkpoint**: PostgreSQL autovacuum or WAL checkpointing.
4. **Garbage collection**: Go GC for Cleat/Temporal, Node GC for DBOS.
5. **Network**: Not applicable for localhost, but relevant for distributed setups.
6. **Temporal**: Dev server vs production server can show different performance
   characteristics. Use the same server mode for all comparisons.

### Reporting checklist

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
- [ ] All four workload patterns tested
- [ ] Variance across runs documented
