# Whole-Cluster Testing, CI/CD, and Comparative Benchmarking Plan

May 2026

---

## Current State

| Area | Status |
|------|--------|
| Unit/benchmark tests | 5 benchmark variants, 4 test suites (fault, concurrency, integration, runner), 2,084 test lines |
| Multi-node testing | None |
| CI/CD pipeline | None (no `.github/workflows/`, no Makefile) |
| Docker Compose | None |
| K8s configs | Basic `deployment.yaml`, `service.yaml`, `configmap.yaml` existing |
| Comparative benchmarks | Cleat side complete (`compare.sh`), Temporal/DBOS runners not implemented |
| Chaos/failure injection | Basic fault test (294 lines), no network partition or disk failure testing |
| Performance regression | None — no historical data, no threshold alerts |

---

## Workstream 1: Local Cluster Testing (P0, ~2 weeks)

### 1a: Docker Compose Multi-Node Cluster

Create a reproducible local cluster that matches a production deployment.

**Create `docker-compose.cluster.yml`:**

```yaml
# 3-worker cleat cluster with PostgreSQL and monitoring
services:
  postgres:
    image: postgres:16
    # shared_buffers, WAL settings for throughput testing
  
  worker-1:
  worker-2:
  worker-3:
    # Each: different task queue, shared DB
  
  dashboard:
    # Web UI on :8080
  
  prometheus:
    # Metrics collection
  
  grafana:
    # Dashboards
```

**Components:**
- 1 PostgreSQL 16 instance (can be scaled to separate instances for testing)
- 3 cleat workers on different task queues
- 1 dashboard instance
- Prometheus + Grafana for metrics collection
- Configurable worker count via `--scale worker=N`

### 1b: Cluster Smoke Test Suite

Tests that run against the multi-node cluster:

```
tests/cluster/
├── cluster_test.go          # Cluster bringup, health checks, worker registration
├── failover_test.go         # Kill worker, verify workflow continues on another
├── replay_test.go           # Crash mid-workflow, verify deterministic replay
├── scale_test.go            # Add/remove workers, verify work redistribution
└── helpers.go               # Docker Compose lifecycle, wait-for-ready, cleanup
```

**Scenarios (minimum):**

| Test | What it validates |
|------|------------------|
| 3 workers register | All workers claim from task queues |
| Spread 100 workflows | Work distributes across workers |
| Kill 1 worker mid-exec | In-flight workflows complete on remaining workers |
| Kill Postgres + restart | Workers reconnect, workflows resume from last event |
| Deploy new WASM version | In-flight workflows use old version, new starts use new |
| Full cluster restart | All state recovered from PostgreSQL |

### 1c: Fault Injection Framework

Extend `internal/host/fault_test.go` with:

```
internal/host/
├── fault_test.go            # Existing (294 lines)
├── fault_network_test.go    # NEW: simulate network partitions
├── fault_disk_test.go       # NEW: simulate slow/full disk
├── fault_clock_test.go      # NEW: clock skew between workers
└── fault_injector.go        # NEW: programmable fault injection
```

**Fault types:**

| Fault | Injection method | Validation |
|-------|-----------------|------------|
| Network partition | iptables DROP between containers | Workflows pause, resume on heal |
| Slow network | tc netem 200ms delay | Heartbeats continue, no false timeouts |
| Postgres restart | docker kill + start | Workers reconnect, no lost events |
| Worker crash-loop | kill -9 in loop | Event history remains consistent |
| Disk full | fallocate / dd | Graceful error, no corruption |
| Clock skew | libfaketime on one worker | Timer-based workflows remain correct |

---

## Workstream 2: CI/CD Pipeline (P0, ~1 week)

### 2a: GitHub Actions Workflow

Create `.github/workflows/ci.yml`:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  lint:
    # ruff (Python), go vet, shellcheck, clippy (Rust)
  
  test-go:
    # matrix: go 1.22, 1.23, 1.24
    # go test ./... -race -count=1
    # Separate job per package for parallelism
  
  test-python:
    # pytest with coverage
    # matrix: python 3.10, 3.11, 3.12
  
  test-java:
    # gradle test
  
  test-assemblyscript:
    # npm test
  
  benchmarks:
    # runs on main push only, stores results
    # go test -bench=. -benchmem ./benchmarks/
    # Compare against previous run, flag regression >5%
  
  cluster-tests:
    # docker-compose up, run cluster smoke tests
    # requires Docker in CI
  
  build:
    # Build all targets: go, python wasm, java, assemblyscript
    # Verify all examples compile
```

### 2b: Makefile

Create `Makefile` at repo root for local CI parity:

```makefile
.PHONY: test test-go test-python test-java test-as test-cluster
.PHONY: lint lint-go lint-python lint-rust lint-sh
.PHONY: bench bench-compare bench-save
.PHONY: build build-go build-python build-java build-as
.PHONY: cluster-up cluster-down cluster-logs
.PHONY: fmt fmt-go fmt-python fmt-rust
.PHONY: all ci

all: lint test build

test: test-go test-python
test-go:
	go test -race -count=1 ./...
test-python:
	cd python-sdk && python -m pytest -v
test-cluster:
	docker-compose -f docker-compose.cluster.yml up -d
	go test -v -count=1 ./tests/cluster/
	docker-compose -f docker-compose.cluster.yml down

bench:
	go test -bench=. -benchmem -benchtime=30s ./benchmarks/

bench-compare:
	./benchmarks/compare.sh

lint: lint-go lint-python lint-sh
lint-go:
	go vet ./...
lint-python:
	cd python-sdk && ruff check .
lint-sh:
	shellcheck **/*.sh

build: build-go build-python
build-go:
	go build ./cmd/...
build-python:
	# Build hello workflow as smoke test
	cd python-sdk && python scripts/build_wasm.py \
		--entry ../examples/python-hello/hello_workflow.py:hello \
		--validate-only

fmt: fmt-go fmt-python
fmt-go:
	go fmt ./...
fmt-python:
	cd python-sdk && ruff format .

cluster-up:
	docker-compose -f docker-compose.cluster.yml up -d
cluster-down:
	docker-compose -f docker-compose.cluster.yml down -v

ci: lint test bench build test-cluster
```

### 2c: Pre-commit Hooks

Create `.pre-commit-config.yaml`:

```yaml
repos:
  - repo: local
    hooks:
      - id: go-vet
        name: go vet
        entry: go vet ./...
        language: system
        pass_filenames: false
      - id: ruff
        name: ruff
        entry: ruff check python-sdk/
        language: system
        pass_filenames: false
      - id: shellcheck
        name: shellcheck
        entry: shellcheck
        types: [shell]
```

### 2d: Benchmark Regression Detection

Create `scripts/benchcmp.sh`:

```bash
#!/bin/bash
# Run benchmarks, compare against baseline, flag regressions > threshold
# Stores baseline in .benchmarks/baseline-<cpu>-<go-version>.txt
# Exits non-zero if any benchmark regressed >5%
```

Store baseline files in `.benchmarks/` (gitignored but CI stores them as artifacts).

---

## Workstream 3: Comparative Benchmarking (P1, ~2 weeks)

### 3a: Standardized Workload Definitions

Define 4 workloads that can be implemented identically in Cleat, Temporal, and DBOS:

```
benchmarks/comparative/
├── README.md                # Methodology, hardware, rules of engagement
├── workflows/
│   ├── 01-sequential/       # N sequential durable steps
│   │   ├── cleat.go
│   │   ├── temporal/        # (Go SDK)
│   │   └── dbos/            # (TypeScript or Go)
│   ├── 02-fanout/           # N parallel child workflows
│   ├── 03-saga/             # N steps + N compensations
│   └── 04-llm-agent/        # Simulated LLM loop with tools
├── results/
│   └── template.md          # Standardized results template
└── runner.sh                # Unified runner for all frameworks
```

**Rules of engagement:**
- Same hardware for all frameworks (documented)
- Same PostgreSQL instance for Cleat and DBOS
- Temporal uses dev server (acknowledged: not apples-to-apples for server overhead)
- Each benchmark: 3 runs, report median
- Warm-up: 10s before measurement
- Measurement window: 60s minimum
- Metrics: throughput (steps/s), P50/P99 latency, DB CPU, memory RSS

### 3b: Temporal Workflow Implementations

Implement the 4 benchmark workloads in Temporal's Go SDK:

```
benchmarks/comparative/workflows/01-sequential/temporal/workflow.go
benchmarks/comparative/workflows/02-fanout/temporal/
benchmarks/comparative/workflows/03-saga/temporal/
benchmarks/comparative/workflows/04-llm-agent/temporal/
```

Each mirrors the Cleat implementation exactly — same number of steps, same data sizes, same sleep durations.

### 3c: DBOS Workflow Implementations

Implement in DBOS (TypeScript, since DBOS's Go SDK is less mature):

```
benchmarks/comparative/workflows/01-sequential/dbos/
benchmarks/comparative/workflows/02-fanout/dbos/
benchmarks/comparative/workflows/03-saga/dbos/
benchmarks/comparative/workflows/04-llm-agent/dbos/
```

### 3d: Automated Comparison Runner

Extend `benchmarks/compare.sh` to:

1. Auto-detect available frameworks (skip if CLI not installed)
2. Run each workload on each available framework
3. Extract metrics programmatically
4. Generate markdown and CSV result files
5. Flag statistically significant differences (>10%)

### 3e: Results Publishing

Output format:

```markdown
## Comparative Results — 2026-05-15

Hardware: AMD Ryzen 5 5500U, 16GB DDR4, NVMe SSD
PostgreSQL 16, shared_buffers=256MB
Go 1.26, Temporal CLI v1.x, DBOS v2.x

| Workload | Cleat (steps/s) | Temporal (steps/s) | DBOS (steps/s) | Cleat vs Temporal | Cleat vs DBOS |
|----------|-----------------|-------------------|----------------|-------------------|---------------|
| Sequential-100 | 88,000,000 | TBD | TBD | TBD | TBD |
| Fanout-100 | 5,200,000 | TBD | TBD | TBD | TBD |
| Saga-50 | ... | TBD | TBD | TBD | TBD |
| LLM-Agent-10 | 5,000,000 | TBD | TBD | TBD | TBD |
```

---

## Workstream 4: Production Hardening Test Suite (P1, ~1 week)

### 4a: Data Integrity Tests

```
tests/integrity/
├── event_history_test.go    # Verify event_history consistency after faults
├── replay_determinism_test.go # Same inputs = same event history hash
├── compaction_test.go       # History compaction preserves correctness
└── concurrent_test.go       # Concurrent workflow updates don't corrupt
```

### 4b: Scale Tests

```
tests/scale/
├── throughput_test.go       # Measure throughput at increasing worker counts
├── latency_test.go          # P50/P99 latency under load
├── connection_test.go       # Behavior under max_connections
├── event_history_growth.go  # Storage growth rate per workflow
└── concurrent_workflows.go  # 10K concurrent workflows
```

**Target metrics to measure:**

| Metric | Test | Baseline |
|--------|------|----------|
| Max throughput (1 worker) | throughput_test | TBD steps/s |
| Max throughput (N workers) | throughput_test | TBD steps/s |
| P50 latency (1-step wf) | latency_test | TBD ms |
| P99 latency (1-step wf) | latency_test | TBD ms |
| Event history growth | event_history_growth | TBD bytes/step |
| Max concurrent workflows | concurrent_workflows | TBD |
| Recovery time (worker crash) | failover_test | TBD ms |

### 4c: Upgrade/Migration Tests

```
tests/upgrade/
├── schema_migration_test.go # Apply all migrations, verify no data loss
├── wasm_version_test.go     # In-flight across multiple WASM versions
└── worker_rolling_test.go   # Rolling worker restart with zero downtime
```

---

## Workstream 5: Observability (P2, ~3 days)

### 5a: Prometheus Metrics

Add Prometheus instrumentation to key code paths:

```go
// Metrics to export
cleat_workflows_started_total
cleat_workflows_completed_total
cleat_workflows_failed_total
cleat_workflows_active
cleat_steps_executed_total
cleat_replay_steps_total
cleat_event_history_size_bytes
cleat_claim_latency_seconds
cleat_wasm_load_latency_seconds
cleat_db_query_latency_seconds
cleat_worker_count
```

### 5b: Grafana Dashboard

Create `monitoring/grafana/cleat-dashboard.json`:
- Workflow throughput over time
- Active workflows gauge
- P50/P99 latency
- Worker health (up/down, last heartbeat)
- Database connection pool
- Event history growth rate
- Error rate by workflow definition

---

## Timeline and Dependencies

```
Week 1: W1a (Docker Compose) + W2a (CI/GitHub Actions)  ← parallel
Week 2: W1b (Cluster tests) + W2b (Makefile) + W4a (Integrity) ← parallel
Week 3: W1c (Fault injection) + W3a-b (Comparative workloads) ← parallel
Week 4: W3c-d (DBOS + runner) + W4b-c (Scale + upgrade) ← parallel
Week 5: W2c-d (Pre-commit + regression) + W5 (Observability) ← parallel
Week 6: Integration, documentation, publish results
```

### Dependencies

```
Docker Compose cluster ──────┬──→ Cluster smoke tests
                              │
                              ├──→ Fault injection tests
                              │
                              └──→ Scale tests

GitHub Actions CI ──→ Pre-commit hooks ──→ Benchmark regression

Comparative workloads ──→ Comparison runner ──→ Results publication

Observability ──→ depends on Docker Compose cluster
```

---

## Files to Create (by workstream)

### W1: Cluster Testing (~8 files)
```
docker-compose.cluster.yml
docker-compose.monitoring.yml   # Prometheus + Grafana
tests/cluster/cluster_test.go
tests/cluster/failover_test.go
tests/cluster/replay_test.go
tests/cluster/scale_test.go
tests/cluster/helpers.go
internal/host/fault_network_test.go
internal/host/fault_disk_test.go
internal/host/fault_clock_test.go
internal/host/fault_injector.go
```

### W2: CI/CD (~6 files)
```
.github/workflows/ci.yml
Makefile
.pre-commit-config.yaml
scripts/benchcmp.sh
scripts/ci-check.sh            # All-in-one local CI check
.gitignore                      # Add .benchmarks/ entries
```

### W3: Comparative (~10 files)
```
benchmarks/comparative/README.md
benchmarks/comparative/workflows/01-sequential/temporal/main.go
benchmarks/comparative/workflows/01-sequential/dbos/main.ts
benchmarks/comparative/workflows/02-fanout/temporal/main.go
benchmarks/comparative/workflows/02-fanout/dbos/main.ts
benchmarks/comparative/workflows/03-saga/temporal/main.go
benchmarks/comparative/workflows/03-saga/dbos/main.ts
benchmarks/comparative/workflows/04-llm-agent/temporal/main.go
benchmarks/comparative/workflows/04-llm-agent/dbos/main.ts
benchmarks/comparative/runner.sh
benchmarks/comparative/results/template.md
```

### W4: Production Hardening (~8 files)
```
tests/integrity/event_history_test.go
tests/integrity/replay_determinism_test.go
tests/integrity/compaction_test.go
tests/integrity/concurrent_test.go
tests/scale/throughput_test.go
tests/scale/latency_test.go
tests/scale/concurrent_workflows_test.go
tests/upgrade/schema_migration_test.go
tests/upgrade/wasm_version_test.go
tests/upgrade/worker_rolling_test.go
```

### W5: Observability (~3 files)
```
monitoring/prometheus/metrics.go    # Prometheus instrumentation
monitoring/grafana/dashboard.json   # Grafana dashboard
monitoring/docker-compose.yml       # Prometheus + Grafana stack
```

---

## Success Criteria

| Week | Deliverable |
|------|-------------|
| 2 | `make ci` passes locally — lint, test, build, cluster smoke |
| 3 | CI green on every PR — all languages, all test suites |
| 4 | Fault injection framework catches at least 3 real bugs |
| 5 | Comparative benchmarks published with Temporal and DBOS numbers |
| 6 | `make cluster-up && make test-cluster` verifies 3-node failover in <5 min |

**End state:** A developer can run `make ci` locally, see their PR get green CI with
benchmark regression detection, and spin up a 3-node cluster with `make cluster-up`
to validate their changes against a realistic deployment. Comparative benchmarks
publish reproducible results proving cleat's performance position.
