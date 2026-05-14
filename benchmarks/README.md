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
