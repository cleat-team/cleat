# Comparative Benchmarking Plan — Cleat vs Temporal vs DBOS

May 2026

---

## Objective

Produce reproducible, defensible, and publishable performance comparisons between
Cleat, Temporal, and DBOS across four representative durable-workflow patterns.
The output is a markdown table and CSV that a reasonable engineer can reproduce
on their own hardware.

---

## 1. What We're Measuring

Four workload patterns, chosen to stress different framework subsystems:

| # | Workload | What It Measures | Why It Matters |
|---|----------|-----------------|----------------|
| 01 | **Sequential steps** | Per-step overhead: event recording, determinism tracking, DB write | Foundation for all workflows; if this is slow, nothing is fast |
| 02 | **Fan-out to N children** | Child workflow spawn + result collection + concurrency management | Map/reduce, scatter/gather, parallel API calls |
| 03 | **Saga with compensation** | Step registration overhead, forward+compensate loop, error-path cost | Microservices orchestration, payment flows, inventory |
| 04 | **LLM agent loop** | Repeated durable calls with varying payloads, tool-use patterns | AI agent workflows (the fastest-growing use case) |

Each workload has a tunable parameter (step count, child count, prompt count)
producing 3–4 configurations per workload, 15 configurations total.

### Configurations tested

```
01-sequential:  steps=10, steps=100, steps=1000
02-fanout:      children=10, children=100, children=500
03-saga:        steps=10, steps=100, steps=1000, fail=10, fail=100
04-llm-agent:   prompts=1/tools=5, prompts=5/tools=3, prompts=10/tools=2, prompts=50/tools=1
```

### Primary metrics

| Metric | Unit | Collection |
|--------|------|------------|
| Throughput | steps/s | Framework-reported (Go benchmark, Temporal metrics, DBOS logs) |
| P50 latency | ms | Per-workflow duration, sorted, median |
| P99 latency | ms | Per-workflow duration, sorted, 99th percentile |
| DB CPU | % | PostgreSQL `pg_stat_activity` or CloudWatch during measurement window |
| Memory RSS | MB | Process RSS at steady state (`/proc/[pid]/status` or `ps`) |
| Wall-clock | s | Total elapsed for fixed-count run |

### Secondary metrics (capture when available)

- P50/P99 claim latency (time from workflow-ready to worker-claim)
- Event history growth rate (bytes per step, DB storage cost)
- Cold-start latency (first workflow after deploy vs steady-state)
- Tail latency at high concurrency (P99.9 at 10K concurrent workflows)

---

## 2. Hardware & Environment

### 2a. Recommended: bare metal or dedicated hosts

Shared-tenancy cloud instances introduce noisy-neighbor variance that
undermines significance. The gold standard:

- **3× dedicated hosts** in the same availability zone + placement group
- Each host: 8+ cores, 32 GB RAM, NVMe SSD
- 10 Gbps network between hosts (no cross-AZ latency)

If bare metal isn't available, use `c5.2xlarge` or equivalent with
constrained CPU credit exhaustion ruled out (unlimited mode, or T-series
avoided entirely).

### 2b. Database

- **PostgreSQL 16** on a dedicated instance (not shared with app)
- RDS `db.r6g.xlarge` (4 vCPU, 32 GB) or equivalent self-hosted
- `shared_buffers = 2 GB`, `wal_level = minimal` (for benchmarks only)
- Provisioned IOPS (3000 baseline, 10000 for scale tests)
- Separate DB instance per framework run to eliminate cross-contamination
- Cleat and DBOS share the same DB during their runs (fair — both use
  PostgreSQL as their primary store)

### 2c. Temporal server (special case)

Temporal requires a separate server component that Cleat and DBOS do not.
This is the hardest fairness problem in the comparison.

Options, in order of defensibility:

1. **Temporal Cloud** (least work, least control): Use Temporal's hosted
   offering, document the tier. Problem: can't control server hardware,
   can't pin to the same AZ.

2. **Self-hosted Temporal with dedicated DB** (recommended): Deploy Temporal
   server (history, matching, frontend services) on 2 instances of the same
   hardware class, with a *separate* PostgreSQL instance for Temporal's
   own store. This is the fairest comparison: all three frameworks get
   equivalent compute. Cost and resource count are higher but documented.

3. **Temporal dev server** (easiest, least fair): Single-process embedded
   server. Only use for development/smoke-testing; never publish results
   from this configuration.

**The plan commits to option 2 for published results.** Option 1 is acceptable
for a first pass if Temporal Cloud is already provisioned.

### 2d. Framework version pinning

| Framework | Version | Notes |
|-----------|---------|-------|
| Cleat | `feature/dev001` branch at commit SHA | Record SHA in results |
| Temporal Server | v1.24.x (latest stable) | Record exact version |
| Temporal Go SDK | v1.25.0 | As pinned in `go.mod` files |
| DBOS | v2.x (latest stable) | Record `npx dbos --version` output |
| Go | 1.26 | Same compiler for Cleat and Temporal |
| Node.js | 22 LTS | For DBOS TypeScript |
| PostgreSQL | 16.x | Same major version for all |

---

## 3. Measurement Methodology

### 3a. Warm-up

- 10-second warm-up phase before measurement begins
- Discard warm-up metrics entirely
- Purpose: JIT compilation, connection pool priming, OS page cache

### 3b. Measurement window

- 60-second minimum measurement window per configuration
- For benchmarks that complete faster (small step counts), loop
  continuously and report aggregate throughput
- Fixed-count mode (10K workflows) for latency distributions

### 3c. Runs and statistical treatment

```
For each (framework, workload, configuration):
  1. Warm up: 10s
  2. Measure: 60s minimum
  3. Cool down: 30s (drain in-flight, reset connection pools)
  4. Repeat steps 1–3 for a total of 5 runs
  5. Discard highest and lowest, report median of remaining 3
  6. Compute coefficient of variation across the 5 runs
     - If CV > 5%: flag as "high variance, investigate"
```

Why 5 runs instead of 3: with only 3 runs, discarding high/low leaves 1 data
point, which isn't a median. Five runs → discard high/low → median of 3.

### 3d. CPU pinning and noise reduction

```bash
# On each benchmark host before running:
sudo cpupower frequency-set --governor performance
echo 0 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo  # if Intel
# Pin benchmark process to specific cores:
taskset -c 2-5 ./run-benchmark
# Isolate those cores from the scheduler:
# (or use cset for stronger isolation)
```

### 3e. What to collect during measurement

```bash
# Per-second samples during the measurement window:
- Framework process RSS (ps -o rss= -p $PID)
- Framework CPU % (ps -o %cpu= -p $PID)
- PostgreSQL CPU % (from pg_stat_activity or pidstat)
- PostgreSQL disk read/write bytes (from /proc/[pg_pid]/io)
- Network bytes (if cross-host: sar -n DEV)
```

Store all raw data. Publish the summarized table but keep raw logs for
reproducibility challenges.

---

## 4. The Fairness Problem (and How We Handle It)

### 4a. Asymmetries

| Concern | Cleat | Temporal | DBOS | Mitigation |
|---------|-------|----------|------|------------|
| Server overhead | None (embedded) | Separate history/matching/frontend services | None (embedded) | Document Temporal server resource usage separately; include in cost-per-throughput calculation |
| Language | Go | Go (SDK) + server (Go) | TypeScript | Acknowledge; same-hardware comparison is still valid because users choose frameworks for their language ecosystem |
| DB usage | Direct PostgreSQL | Temporal's own DB + user DB (if separate) | Direct PostgreSQL | Give Temporal its own DB; report both DBs' resource usage |
| Client-server RTT | In-process (0 μs) | gRPC to server (~100–500 μs) | In-process (0 μs) | This IS the architecture difference we're measuring; don't normalize it away |
| Maturity | Pre-1.0 | Production since 2020 | Production since 2023 | Acknowledge in interpretation; flag that Cleat may optimize further |

### 4b. What we will NOT do

- **Normalize for language**: Cleat and Temporal use Go, DBOS uses TypeScript.
  We won't implement a Go version of DBOS workflows just to remove language
  from the equation — users pick a framework + language together.

- **Normalize for server overhead**: Temporal's architecture is what it is.
  We'll report the server's resource usage alongside Temporal's numbers so
  readers can judge total cost of ownership.

- **Claim "X is faster" without qualification**: Every result will include
  context: hardware, versions, configuration, and caveats.

### 4c. How we'll present it

The results table will include footnotes:

```
* Temporal server: 2x c5.2xlarge + 1x db.r6g.xlarge (dedicated).
  Cleat and DBOS use no separate server. Server resource usage
  during benchmark: CPU 40%, RSS 2.1 GB.
† DBOS is TypeScript (Node.js 22). Cleat and Temporal are Go.
  Language overhead is acknowledged but not normalized.
```

---

## 5. Implementation Plan

### 5a. What already exists

All workload implementations, `runner.sh`, and methodology docs were created
in the `cluster-testing-ci-benchmarks` workstream (May 2026). The code is at:

```
benchmarks/comparative/
├── README.md
├── runner.sh                          # Local comparison runner
├── results/template.md
└── workflows/
    ├── 01-sequential/{temporal/main.go, dbos/main.ts}
    ├── 02-fanout/{temporal/main.go, dbos/main.ts}
    ├── 03-saga/{temporal/main.go, dbos/main.ts}
    └── 04-llm-agent/{temporal/main.go, dbos/main.ts}
```

The Cleat implementations already exist at `benchmarks/workflows/` with
the benchmark harness at `benchmarks/cleat_bench_test.go`.

### 5b. What still needs to be built

| Phase | Work | Effort | Owner |
|-------|------|--------|-------|
| **1. Cloud infra** | Terraform for 3-framework provisioning (EC2 or bare metal, RDS, security groups, placement groups) | ~3 days | |
| **2. Temporal setup** | Self-hosted Temporal server deployment + configuration + DB provisioning | ~2 days | |
| **3. DBOS setup** | Node.js + DBOS SDK installation, DB connection, verify workflows run | ~1 day | |
| **4. Remote runner** | Extend `runner.sh` to SSH into remote clusters, upload binaries, collect metrics | ~2 days | |
| **5. Metric collection** | `pidstat`/`/proc` sampling during measurement, CloudWatch/RDS metrics API | ~1 day | |
| **6. Dry run** | Run all 15 configs × 3 frameworks, fix failures, tune measurement parameters | ~2 days | |
| **7. Production run** | 5 runs per config × 15 configs × 3 frameworks = 225 measurement windows (~4–6 hours wall clock with parallelization) | ~1 day | |
| **8. Analysis** | Compute medians, CVs, generate final markdown + CSV, write narrative | ~2 days | |
| **9. Review** | Internal review for methodology soundness, fairness of presentation | ~1 day | |
| **10. Publish** | Publish results markdown in repo, cross-link from README | ~0.5 day | |

**Total: ~15 working days** (3 calendar weeks with parallelization).

### 5c. Terraform sketch

```
infra/comparative-benchmarks/
├── main.tf                 # Provider, VPC, subnets, placement group
├── security-groups.tf      # Per-framework SG rules
├── cleat.tf                # EC2 launch template, RDS instance
├── temporal.tf             # EC2 (server + worker), RDS instances
├── dbos.tf                 # EC2 launch template, RDS instance (shared with cleat)
├── outputs.tf              # Public IPs, connection strings
└── variables.tf            # Instance types, AMI, region
```

Design principle: **provision all three stacks from the same Terraform run**.
This guarantees identical AMI, subnet placement, and instance type.

### 5d. Remote runner design

The `cloud-runner.sh` (new file, extends the existing `runner.sh`):

```
1. Read outputs from Terraform (IPs, connection strings)
2. For each framework:
   a. SCP benchmark binary + workflow definitions to target host
   b. SSH: start framework server/worker (background)
   c. SSH: wait for health check
   d. SSH: run benchmark with taskset + metric collection wrapper
   e. SCP: pull raw results + metrics back
   f. SSH: stop framework server/worker
3. Compute medians, generate markdown + CSV
4. Print results table
```

---

## 6. Results Format

### 6a. Published table (example — with placeholder numbers)

```
## Comparative Results — 2026-06-15

Hardware: 3× c5.2xlarge (8 vCPU, 16 GB), same AZ + placement group
Database: RDS PostgreSQL 16, db.r6g.xlarge (4 vCPU, 32 GB), 10000 IOPS
Temporal Server: 2× c5.2xlarge (separate from workers) + dedicated RDS
Go 1.26, Temporal Server v1.24.3, Temporal Go SDK v1.25.0, DBOS v2.X, Node.js 22

### Throughput (steps/s, higher is better)

| Workload | Cleat | Temporal* | DBOS† | Cleat vs Temporal | Cleat vs DBOS |
|----------|-------|-----------|-------|-------------------|---------------|
| Sequential-10 | 48,600,000 | TBD | TBD | TBD | TBD |
| Sequential-100 | 8,500,000 | TBD | TBD | TBD | TBD |
| Sequential-1000 | 880,000 | TBD | TBD | TBD | TBD |
| Fanout-10 | 66,800 | TBD | TBD | TBD | TBD |
| Fanout-100 | 6,700 | TBD | TBD | TBD | TBD |
| Fanout-500 | 1,340 | TBD | TBD | TBD | TBD |
| Saga-10 | 42,000,000 | TBD | TBD | TBD | TBD |
| Saga-100 | 4,200,000 | TBD | TBD | TBD | TBD |
| Saga-1000 | 420,000 | TBD | TBD | TBD | TBD |
| Saga-fail-10 | 21,000,000 | TBD | TBD | TBD | TBD |
| Saga-fail-100 | 2,100,000 | TBD | TBD | TBD | TBD |
| LLM-1×5 | 12,500,000 | TBD | TBD | TBD | TBD |
| LLM-5×3 | 8,300,000 | TBD | TBD | TBD | TBD |
| LLM-10×2 | 6,250,000 | TBD | TBD | TBD | TBD |
| LLM-50×1 | 3,100,000 | TBD | TBD | TBD | TBD |

* Temporal numbers include gRPC overhead to Temporal server.
  Server resource usage: CPU 40%, RSS 2.1 GB (not included in worker metrics).
† DBOS is TypeScript (Node.js 22). Cleat and Temporal are Go.

### P50 Latency (ms, lower is better)

| Workload | Cleat | Temporal | DBOS |
|----------|-------|----------|------|
| Sequential-10 | TBD | TBD | TBD |
| Sequential-100 | TBD | TBD | TBD |
| ... | ... | ... | ... |

### Resource Usage at Steady State

| Framework | Worker CPU | Worker RSS | DB CPU | Total Cost/hr* |
|-----------|-----------|------------|--------|----------------|
| Cleat | TBD% | TBD MB | TBD% | $TBD |
| Temporal | TBD% | TBD MB | TBD% + server DB | $TBD |
| DBOS | TBD% | TBD MB | TBD% | $TBD |

* On-demand EC2 + RDS pricing for the benchmark configuration.
  Not a TCO analysis, just the cost of the measured infrastructure.
```

### 6b. Versioning results

Results are versioned by date and commit SHA:

```
benchmarks/comparative/results/
├── results-2026-06-15.md
├── results-2026-06-15.csv
├── results-2026-09-01.md    # Next run after optimizations
├── results-2026-09-01.csv
└── raw/                      # Raw benchmark output, not committed
    └── .gitkeep
```

The markdown files are committed to the repo. Raw data is stored as CI
artifacts or in S3.

---

## 7. Threats to Validity

| Threat | Severity | Mitigation |
|--------|----------|------------|
| **Noisy neighbors** (cloud) | High | Dedicated hosts or multiple runs at different times of day; report CV |
| **Temporal server overhead** | Medium | Measure it separately; include server resource usage in results |
| **DBOS language penalty** (TypeScript vs Go) | Medium | Acknowledge prominently; the comparison is "what users actually get" not "what's theoretically possible" |
| **Warm-up insufficiency** | Medium | 10s minimum; verify JIT/GC has stabilized by checking metric stability during warm-up |
| **Configuration differences** (DB tuning, GC settings) | Medium | Document all non-default config; use framework defaults unless they're clearly wrong for benchmarking |
| **Benchmark implementation differences** | Low | Each implementation reviewed to ensure same number of steps, same data sizes, same sleep durations |
| **Single-AZ placement** (not representative of geo-distributed) | Low | Document; this is a single-region throughput test, not a geo-replication test |
| **Workloads are synthetic** | Low | Acknowledge; real workloads are messier but synthetic workloads isolate framework overhead cleanly |

---

## 8. Success Criteria

| Criterion | Threshold |
|-----------|-----------|
| All 15 configs × 3 frameworks complete | 45 measurement sets |
| Coefficient of variation < 5% for primary metrics | Per-config |
| Results reproducible by independent party | Documented hardware + pinned versions + provided scripts |
| Temporal server resource usage documented | CPU, RSS, DB CPU during measurement |
| Cost comparison included | EC2 + RDS on-demand pricing for benchmark configuration |
| Results published in repo | `benchmarks/comparative/results/` |
| Link from root README | "Comparative Benchmarks" section |

---

## 9. Timeline

```
Week 1: Cloud infrastructure (Terraform, provision clusters)
Week 2: Temporal setup + DBOS setup + remote runner
Week 3: Dry runs, tuning, production run, analysis
Week 4: Review, publish, cross-link
```

Each week assumes one person working on this full-time. If multiple people
are available, phases 1 and 2 can partially overlap (one person on infra,
another on the remote runner).
