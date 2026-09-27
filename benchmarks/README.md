# Cleat benchmarks

Micro-benchmarks of cleat's own internals. They answer one question: **did this
commit make cleat slower?**

Cross-framework comparison — cleat vs Temporal vs DBOS — lives in
[`cleat-team/cleat-bench`](https://github.com/cleat-team/cleat-bench), which has
the runners for all three frameworks, seven workload specs, AWS infrastructure,
and cost analysis. This directory previously carried a second, weaker copy of
that harness under `comparative/`: four workloads, no infrastructure, and no
result ever produced in the year it existed. It was removed rather than
maintained in parallel.

## The three suites, and why they are in different modules

| Path | Module | Imports | Measures |
|---|---|---|---|
| `wasm_bench_test.go` | root | `engine` | WASM compile, instantiate, and execute cost |
| `db_bench_test.go` | root | `engine` | Claim queries, event-history writes, against real PostgreSQL |
| `workflows/` | `benchmarks/workflows` | `cleat/` SDK | In-process `HostCalls` throughput, no WASM |

The split is deliberate, not incidental. The root module must not depend on the
`cleat/` SDK — `go list -deps ./... \| grep -c cleat-team/cleat/cleat` must be 0,
and `TestRootModuleDoesNotDependOnSDK` fails if that changes. So the benchmarks
that need the SDK live in their own module, supplied through `go.work`, and the
ones that only need `engine` stay in the root module.

## Running

```bash
# WASM benchmarks — no external tooling required
go test -bench=. -benchmem -benchtime=30s ./benchmarks/

# One benchmark
go test -bench=BenchmarkWorkflowExecute -benchtime=10s ./benchmarks/

# In-process SDK benchmarks — separate module
cd benchmarks/workflows && go test -bench=. -benchmem -benchtime=10s ./...

# Database benchmarks — build-tagged, needs a real PostgreSQL
export CLEAT_DB_BENCH_DSN="postgres://user:pass@localhost:5432/cleat_bench?sslmode=disable"
go test -tags=db_bench -bench=. -benchmem -benchtime=10s ./benchmarks/
```

`make bench` runs the first of these; `make bench-save` writes the output to
`.benchmarks/` keyed by architecture and Go version, for comparison across
commits.

## Metrics

Reported via `testing.B.ReportMetric`:

| Metric | Meaning |
|---|---|
| `wf/s` | Workflow completions per second |
| `steps/s` | Durable API calls per second |
| `ns/op` | Wall-clock time per workflow |
| `B/op`, `allocs/op` | Allocation size and count per workflow, with `-benchmem` |

## Reading the numbers honestly

`workflows/` uses an in-process `HostCalls` whose durable calls return
immediately with no database, no persistence, and no WASM. Its throughput
figures measure Go function-call cost, and they are three to five orders of
magnitude above what a durable workflow achieves against real PostgreSQL. They
are useful for detecting a regression between two commits of cleat. They are not
a throughput claim, and quoting them as one has caused real damage to this
project's documentation before — see `DX_COMPARISON.md`.

For numbers that mean something externally, use `cleat-bench`.

## Variance

- **Within a run** (< 2%) — normal GC and scheduler jitter.
- **Between runs** (2–5%) — expected without careful isolation. Run three times, report the median.
- **Above 5%** — the environment, not the code. Check the `performance` CPU governor and that Turbo Boost is off; check for thermal throttling; check `pg_stat_activity` for autovacuum during a `db_bench` run; pin with `taskset`; lengthen `-benchtime`.

```bash
sudo cpupower frequency-set --governor performance
echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
```

When recording a result, include the cleat commit SHA, `go version`, CPU model,
RAM, disk type, kernel, and — for `db_bench` — the PostgreSQL version and
`shared_buffers` / `work_mem` / `max_connections`.

## Adding a benchmark

1. Decide which module it belongs in, using the table above: does it need the
   `cleat/` SDK, or only `engine`?
2. Add it alongside the existing benchmarks in that module, following their
   naming and sub-benchmark configuration style.
3. Report `wf/s` and `steps/s` via `b.ReportMetric` so output stays comparable.
4. If the pattern is also worth comparing across frameworks, add a workload spec
   to `cleat-bench` — not here.
