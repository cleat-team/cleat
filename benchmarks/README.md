# Cleat vs Temporal vs DBOS: Durable Workflow Benchmarks

Reproducible performance benchmarks comparing Cleat, Temporal, and DBOS
across four workflow patterns.

## Workloads

| Benchmark pattern      | File                          | What it measures                                      |
|------------------------|-------------------------------|-------------------------------------------------------|
| Simple sequential      | `workflows/simple.go`         | N sequential steps, pure framework overhead           |
| Fan-out children       | `workflows/fanout.go`         | N child workflows spawned and awaited in parallel     |
| Saga compensation      | `workflows/saga.go`           | N steps with compensation handlers (happy + failure)  |
| AI agent loop          | `workflows/llm.go`            | LLM chat call + tool invocations per turn             |

## How to run (Cleat)

```bash
# From repo root
go test -bench=. -benchmem -benchtime=30s ./benchmarks/
```

To run a single benchmark:

```bash
go test -bench=BenchmarkSimpleWorkflow/steps=100 -benchtime=10s ./benchmarks/
```

## Hardware to record

When publishing results, include:

- **CPU**: model, core count, frequency, turbo enabled?
- **RAM**: total, type (DDR4/DDR5), speed
- **Disk**: NVMe vs SATA SSD, model, filesystem
- **OS**: kernel version, distribution
- **Go**: `go version` output
- **PostgreSQL** (if applicable): version, config (`shared_buffers`, `max_connections`)

### Recommended hardware

For reproducible results, use a dedicated instance with no other workloads:

- **AWS i4i.2xlarge** (8 vCPUs, 64 GB RAM, 1 x 950 GB NVMe SSD) or equivalent
  bare-metal machine
- **CPU**: Intel Xeon (Ice Lake) at 3.5 GHz sustained, Turbo Boost disabled
- **RAM**: 64 GB DDR4
- **Disk**: NVMe SSD with XFS or ext4
- **OS**: Ubuntu 22.04 LTS or later, kernel 6.x
- **Go**: 1.25+
- **PostgreSQL**: 16.x with `shared_buffers = 16GB`, `work_mem = 64MB`

Disable CPU frequency scaling and Turbo Boost before benchmarking:

```bash
sudo cpupower frequency-set --governor performance
echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
```

## Metrics collected

Each benchmark reports (via `testing.B.ReportMetric`):

| Metric     | Unit   | Meaning                                               |
|------------|--------|-------------------------------------------------------|
| `wf/s`     | ops    | Workflow completions per second                       |
| `steps/s`  | ops    | Durable API calls per second (steps/sec throughput)   |
| `ns/op`    | ns     | Nanoseconds per single workflow (standard Go metric)  |
| `B/op`     | bytes  | Allocations per workflow (with `-benchmem`)            |
| `allocs/op`| allocs | Allocation count per workflow (with `-benchmem`)       |

Additional metrics to collect manually:

- **DB CPU**: PostgreSQL process CPU usage during the run (`pg_stat_activity`)
- **Memory**: RSS of the benchmark process (`/usr/bin/time -v`)
- **P50 / P99 latency**: single-workflow tail latency (requires `-benchtime=1` run)

## How to compare with Temporal dev server

1. Start Temporal dev server:
   ```bash
   temporal server start-dev --db-file /tmp/temporal.db
   ```

2. Port the workflow definitions in `benchmarks/workflows/*.go` to the
   Temporal Go SDK using `workflow.ExecuteActivity` / `workflow.ChildWorkflow`.

3. Run the Temporal equivalent of the benchmarks:
   ```bash
   # In your Temporal benchmark project
   go test -bench=. -benchtime=30s ./... -temporal-host localhost:7233
   ```

4. Collect the same metrics from Temporal's Web UI and server metrics.

## How to compare with DBOS

1. Start DBOS local development server:
   ```bash
   dbos local-start
   ```

2. Port the workflow definitions to the DBOS TypeScript SDK using
   `DBOS.workflow()` / `DBOS.step()` / `DBOS.send()`.

3. Run the DBOS equivalent benchmarks:
   ```bash
   npx jest --bench --benchtime=30s
   ```

4. Collect DB CPU and memory from PostgreSQL and the DBOS process.

## Methodology notes

- **Warm-up**: `testing.B` automatically ramps `b.N` to reach the target
  `-benchtime`. The first few iterations are warm-up.

- **Isolation**: Run each framework on a dedicated machine or container with
  no other load. Pin to a single CPU core (`taskset -c 0`) for
  single-threaded comparisons.

- **Repeat**: Run each benchmark 3 times and report median. Variance above
  5% indicates insufficient isolation.

- **Noise**: Disable CPU scaling, Turbo Boost, and ASLR for the most
  repeatable results:
  ```bash
  sudo cpupower frequency-set --governor performance
  echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
  ```

## How to contribute new benchmark scenarios

New benchmark scenarios follow a four-step process:

### 1. Define the workflow pattern

Create a new file in `benchmarks/workflows/` that implements the pattern.
Follow the conventions in existing benchmarks:

- The workflow must accept a `*testing.B` and a configuration parameter.
- Use the `engine.NewEngine()` API (or the in-process test harness) to
  isolate framework overhead.
- Register all service calls as no-op stubs that return immediately.
- Name the file after the pattern (e.g. `accumulator.go`, `nested.go`).

```go
// Example skeleton:
func BenchmarkAccumulator(b *testing.B, steps int) {
    for i := 0; i < b.N; i++ {
        // ... workflow logic ...
    }
}
```

### 2. Register the scenario

Add the new benchmark function to the test table in `benchmarks/bench_test.go`.
Follow the existing pattern of defining configurations (step counts,
fan-out sizes, etc.):

```go
// In benchmarks/bench_test.go
{"Accumulator", "%s/steps=%d", []int{10, 100}},
```

### 3. Port to comparison frameworks

For Temporal and DBOS comparisons, port the same pattern to each framework's
SDK:

- **Temporal**: `benchmarks/comparative/workflows/<pattern>/temporal/`
- **DBOS**: `benchmarks/comparative/workflows/<pattern>/dbos/`

Include a short README per comparison directory with framework-specific
build and run instructions.

### 4. Run and validate

Execute the new benchmark and verify the output format:

```bash
go test -bench=BenchmarkAccumulator -benchtime=10s ./benchmarks/
```

Ensure the output includes `wf/s` and `steps/s` metrics. Verify that ported
Temporal and DBOS versions produce structurally identical results (same
configuration parameters, same measurement windows, same concurrency model).

## Result interpretation and variance troubleshooting

### Reading the metrics

| Metric | What it tells you |
|--------|-------------------|
| `wf/s` | End-to-end throughput for the whole workflow. Higher is better. |
| `steps/s` | Durable API call throughput. Higher means the framework handles per-call overhead efficiently. |
| `ns/op` | Wall-clock time per workflow. Lower is better. |
| `B/op` | Memory allocated per workflow. High values may indicate history-buffer bloat. |

### Expected variance

- **Within-run** (< 2%): normal jitter from Go GC and OS scheduling.
- **Between-run** (2-5%): expected when runs are not perfectly isolated.
  Run 3 times and report the median.
- **Between-machine** (5-20%): differences in CPU model, RAM speed, or
  PostgreSQL configuration. Normalise by including full hardware specs
  (see [Hardware to record](#hardware-to-record)).
- **Between-day** (10-30%): OS updates, autovacuum, or SSD wear. Re-run
  the full suite if publishing comparative results.

### Troubleshooting high variance

If variance exceeds 5% across three consecutive runs:

1. **Check CPU governance**: `cpupower frequency-info` should show
   `performance` governor. Turbo Boost must be disabled.
2. **Check thermal throttling**: `sensors` or `turbostat` should not show
   frequency drops during the benchmark window.
3. **Check PostgreSQL activity**: Run `SELECT * FROM pg_stat_activity`
   during the benchmark. Autovacuum or concurrent queries add noise.
4. **Check background processes**: `top`, `iotop`, and `nethogs` should
   show only the benchmark and PostgreSQL processes.
5. **Pin to dedicated cores**: `taskset -c 0-3` for the benchmark and
   `taskset -c 4-7` for PostgreSQL. This eliminates context-switching.
6. **Increase benchmark time**: From 30s to 60s or 120s. Longer windows
   smooth out GC pauses and OS scheduling jitter.
7. **Add warm-up**: Include 10-30 seconds of warm-up before the measurement
   window to stabilise JIT compilation and database query plans.

If variance persists, consider whether the benchmark pattern itself is
allocation-heavy (many string concatenations, large JSON payloads), making
it more sensitive to GC pressure.

### Reporting results

When publishing results, include:

- Raw output from all three runs (not just the median).
- Hardware specs using the [checklist above](#hardware-to-record).
- The Cleat engine commit SHA under test.
- PostgreSQL configuration (`shared_buffers`, `work_mem`, `max_connections`).
- Any deviation from standard methodology (different `-benchtime`,
  different concurrency settings).

## Expected output

```
goos: linux
goarch: amd64
pkg: github.com/cleat-team/cleat/benchmarks
cpu: Intel(R) Xeon(R) Gold 6438M
BenchmarkSimpleWorkflow/steps=10-128         	   59320	     20210 ns/op	  48577 wf/s	485766 steps/s
BenchmarkSimpleWorkflow/steps=100-128         	   10120	    118200 ns/op	   8460 wf/s	 84600 steps/s
BenchmarkFanOutWorkflow/children=10-128       	    3820	    315000 ns/op	   3175 wf/s	 66750 steps/s
BenchmarkSagaWorkflow/steps=10-128            	   50100	     23800 ns/op	  42017 wf/s	420168 steps/s
BenchmarkLLMCallWorkflow/prompts=1_tools=5-128	   25030	     47800 ns/op	  20920 wf/s	125521 steps/s
```

(Actual numbers will vary by hardware.)
