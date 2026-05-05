// Performance and scalability analysis.
//
// Build & run:
//   GOTOOLCHAIN=local /home/rcownie/go/bin/go build -o /tmp/perf ./cluster/performance.go
//   /tmp/perf

package main

import (
	"fmt"
	"strings"
)

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Performance & Scalability Analysis")
	fmt.Println(strings.Repeat("=", 72))

	concurrencyModel()
	costAnalysis()
	scalingLimits()
	hardLimits()
	stagedScaling()
	comparisonToTemporal()
}

func concurrencyModel() {
	fmt.Println()
	fmt.Println("── 1. CONCURRENCY MODEL ──")
	fmt.Println()
	fmt.Println("  A worker runs N goroutines, each driving one workflow via")
	fmt.Println("  wazero WASM runtime. The bottleneck is NOT CPU — workflows")
	fmt.Println("  are I/O-bound, waiting on HTTP calls (10-500ms each) or")
	fmt.Println("  human steps (hours/days).")
	fmt.Println()
	fmt.Println("  Per-workflow resources:")
	fmt.Println()

	// Memory model for a typical workflow.
	const (
		goroutineStack = 4 * 1024       // 4 KB (Go runtime, grows as needed)
		wasmMemory     = 2 * 1024 * 1024 // 2 MB (typical tinygo WASM module heap)
		hostOverhead   = 1 * 1024 * 1024 // 1 MB (host structs, event history buffers)
		totalPerWF     = goroutineStack + wasmMemory + hostOverhead
	)

	fmt.Printf("    Goroutine stack:        %d KB\n", goroutineStack/1024)
	fmt.Printf("    WASM module memory:     %d MB (tinygo heap)\n", wasmMemory/(1024*1024))
	fmt.Printf("    Host overhead:          %d MB (buffers, state)\n", hostOverhead/(1024*1024))
	fmt.Printf("    ─────────────────────────────────────\n")
	fmt.Printf("    Total per workflow:     ~%.1f MB\n", float64(totalPerWF)/(1024*1024))
	fmt.Println()

	// Concurrent capacity for different worker sizes.
	workerSizes := []struct {
		ramGB       int
		description string
		costPerHr   float64
	}{
		{4, "t3.medium / c6i.large", 0.042},
		{8, "t3.large / c6i.xlarge", 0.083},
		{16, "t3.xlarge / c6i.2xlarge", 0.166},
		{32, "c6i.4xlarge", 0.332},
		{64, "c6i.8xlarge", 0.665},
	}

	fmt.Println("  Concurrent workflows per worker (80%% RAM utilization):")
	fmt.Println()
	fmt.Println("  ┌──────────┬─────────────────────┬───────────┬─────────────┐")
	fmt.Println("  │ RAM      │ Example instance    │ Max wfs   │ Cost/hr      │")
	fmt.Println("  ├──────────┼─────────────────────┼───────────┼─────────────┤")
	for _, s := range workerSizes {
		usableRAM := int(float64(s.ramGB*1024*1024*1024) * 0.8)
		maxWFs := usableRAM / totalPerWF
		fmt.Printf("  │ %2d GB    │ %-19s │ %-9d │ $%-10.3f  │\n",
			s.ramGB, s.description, maxWFs, s.costPerHr)
	}
	fmt.Println("  └──────────┴─────────────────────┴───────────┴─────────────┘")
	fmt.Println()
	fmt.Println("  Note: these are CONCURRENT (in-flight) workflows, not")
	fmt.Println("  completed/second. A workflow waiting 3 days for human")
	fmt.Println("  approval consumes memory but near-zero CPU.")
	fmt.Println()
}

func costAnalysis() {
	fmt.Println("── 2. COST ANALYSIS ──")
	fmt.Println()
	fmt.Println("  Approximate cloud pricing (AWS us-east-1, on-demand, mid-2026):")
	fmt.Println()

	// Pricing data.
	const (
		pgSmallHr = 0.25  // RDS db.r6g.large, 2 vCPU, 16 GB, $0.25/hr
		pgMedHr   = 0.50  // RDS db.r6g.xlarge, 4 vCPU, 32 GB
		pgLargeHr = 1.00  // RDS db.r6g.2xlarge, 8 vCPU, 64 GB
		pgXLargeHr = 2.00 // RDS db.r6g.4xlarge, 16 vCPU, 128 GB

		workerSmallHr = 0.042 // t3.medium, 2 vCPU, 4 GB
		workerMedHr   = 0.083 // t3.large, 2 vCPU, 8 GB
		workerLargeHr = 0.166 // t3.xlarge, 4 vCPU, 16 GB

		storagePerGBMo = 0.08 // GP3 SSD
		dataXferPerGB  = 0.09 // inter-AZ
	)

	scenarios := []struct {
		name        string
		description string
		pgCostHr    float64
		workerCount int
		workerCostHr float64
		workersRAM  int
		maxConcurrent int
		storageGB   int
		stepsPerSec int
	}{
		{
			name: "Development", description: "Single worker, small PG, 10 GB storage",
			pgCostHr: pgSmallHr / 2, workerCount: 1, workerCostHr: workerSmallHr,
			workersRAM: 4, maxConcurrent: 100, storageGB: 10, stepsPerSec: 50,
		},
		{
			name: "Small Production", description: "3 workers, medium PG, 100 GB storage",
			pgCostHr: pgSmallHr, workerCount: 3, workerCostHr: workerMedHr,
			workersRAM: 8, maxConcurrent: 3000, storageGB: 100, stepsPerSec: 500,
		},
		{
			name: "Medium Production", description: "10 workers, large PG, 500 GB storage",
			pgCostHr: pgMedHr, workerCount: 10, workerCostHr: workerMedHr,
			workersRAM: 8, maxConcurrent: 10000, storageGB: 500, stepsPerSec: 2000,
		},
		{
			name: "Large Production", description: "30 workers, xlarge PG, 2 TB storage",
			pgCostHr: pgLargeHr, workerCount: 30, workerCostHr: workerLargeHr,
			workersRAM: 16, maxConcurrent: 60000, storageGB: 2000, stepsPerSec: 8000,
		},
		{
			name: "Very Large", description: "100 workers, 16xlarge PG, 10 TB storage",
			pgCostHr: pgXLargeHr, workerCount: 100, workerCostHr: workerLargeHr,
			workersRAM: 16, maxConcurrent: 200000, storageGB: 10000, stepsPerSec: 20000,
		},
	}

	_ = storagePerGBMo
	_ = dataXferPerGB

	fmt.Println("  ┌─────────────────┬───────┬────────────┬─────────────┬──────────┐")
	fmt.Println("  │ Scenario         │ PG/mo │ Workers/mo │ Storage/mo  │ Total/mo │")
	fmt.Println("  ├─────────────────┼───────┼────────────┼─────────────┼──────────┤")
	for _, s := range scenarios {
		pgMo := s.pgCostHr * 730
		workersMo := s.workerCostHr * float64(s.workerCount) * 730
		storageMo := float64(s.storageGB) * storagePerGBMo
		totalMo := pgMo + workersMo + storageMo + 10 // +$10 for data transfer

		fmt.Printf("  │ %-15s │ $%4.0f  │ $%8.0f  │ $%9.0f  │ $%7.0f │\n",
			s.name, pgMo, workersMo, storageMo, totalMo)
	}
	fmt.Println("  └─────────────────┴───────┴────────────┴─────────────┴──────────┘")
	fmt.Println()

	fmt.Println("  Additional context for each tier:")
	fmt.Println()
	for _, s := range scenarios {
		fmt.Printf("  %s:\n", s.name)
		fmt.Printf("    Concurrent workflows: ~%d\n", s.maxConcurrent)
		fmt.Printf("    Durable calls/sec:    ~%d (INSERTs into event_history)\n", s.stepsPerSec)
		fmt.Printf("    Workers:              %d × %d GB RAM\n", s.workerCount, s.workersRAM)
		fmt.Println()
	}

	fmt.Println("  Cost per workflow (amortized):")
	fmt.Println()
	fmt.Println("  ┌─────────────────┬──────────────┬──────────────────┐")
	fmt.Println("  │ Scenario         │ $/wf-hour★   │ $/1K wf-completed│")
	fmt.Println("  ├─────────────────┼──────────────┼──────────────────┤")

	wfCosts := []struct {
		name       string
		totalPerHr float64
		maxConc    int
	}{
		{"Development", (pgSmallHr/2 + workerSmallHr), 100},
		{"Small Prod", (pgSmallHr + 3*workerMedHr), 3000},
		{"Medium Prod", (pgMedHr + 10*workerMedHr), 10000},
		{"Large Prod", (pgLargeHr + 30*workerLargeHr), 60000},
		{"Very Large", (pgXLargeHr + 100*workerLargeHr), 200000},
	}
	for _, wc := range wfCosts {
		perWFHr := wc.totalPerHr / float64(wc.maxConc)
		per1K := perWFHr * 1000 * 0.1 // assuming avg workflow takes ~6 minutes
		fmt.Printf("  │ %-15s │ $%-11.6f │ $%-15.4f │\n",
			wc.name, perWFHr, per1K)
	}
	fmt.Println("  └─────────────────┴──────────────┴──────────────────┘")
	fmt.Println("  ★ cost per concurrent-workflow-hour (mostly RAM)")
	fmt.Println("  ★ per-1K assumes avg workflow takes ~6 min wall-clock")
	fmt.Println()
}

func scalingLimits() {
	fmt.Println("── 3. WHERE ARE THE BOTTLENECKS? ──")
	fmt.Println()

	const (
		pgInsertsPerSec    = 20000 // Single PG instance, modest hardware
		pgInsertsPerSecBig = 100000 // High-end PG with tuned config
		pgClaimsPerSec     = 1000  // SKIP LOCKED dequeue
		pgClaimsPerSecBig  = 5000 // With connection pooling + tuning
		stepsPerWorkflow   = 8     // Average (catalog + inventory + payment + shipping + notification + error handling)
	)

	fmt.Println("  Bottleneck 1: Event history INSERT throughput")
	fmt.Println("  ───────────────────────────────────────────────")
	fmt.Println()
	fmt.Printf("    PostgreSQL INSERTs/sec (modest):   %d\n", pgInsertsPerSec)
	fmt.Printf("    PostgreSQL INSERTs/sec (tuned):    %d\n", pgInsertsPerSecBig)
	fmt.Printf("    Average steps per workflow:         %d\n", stepsPerWorkflow)
	fmt.Printf("    Max workflow completions/sec:       %d (modest), %d (tuned)\n",
		pgInsertsPerSec/stepsPerWorkflow, pgInsertsPerSecBig/stepsPerWorkflow)
	fmt.Printf("    Max workflow completions/day:       %d (modest), %.1fM (tuned)\n",
		(pgInsertsPerSec/stepsPerWorkflow)*86400,
		float64(pgInsertsPerSecBig/stepsPerWorkflow)*86400/1e6)
	fmt.Println()

	fmt.Println("  Bottleneck 2: Work queue claim throughput")
	fmt.Println("  ───────────────────────────────────────────")
	fmt.Println()
	fmt.Printf("    PG SKIP LOCKED claims/sec (modest): %d\n", pgClaimsPerSec)
	fmt.Printf("    PG SKIP LOCKED claims/sec (tuned):  %d\n", pgClaimsPerSecBig)
	fmt.Println("    Note: claims happen once per workflow execution,")
	fmt.Println("    not once per step. 1 claim → N steps → 1 completion.")
	fmt.Println("    So claim throughput is rarely the bottleneck.")
	fmt.Println()

	fmt.Println("  Bottleneck 3: Worker memory")
	fmt.Println("  ───────────────────────────")
	fmt.Println()
	memPerWF := 4*1024 + 2*1024*1024 + 1*1024*1024 // ~3MB per workflow
	fmt.Printf("    Memory per concurrent workflow:  ~%.1f MB\n", float64(memPerWF)/(1024*1024))
	fmt.Printf("    Workers needed for 10K concurrent: %.0f (8 GB each)\n",
		float64(10000)*float64(memPerWF)/(8*1024*1024*1024*0.8))
	fmt.Printf("    Workers needed for 100K concurrent: %.0f (8 GB each)\n",
		float64(100000)*float64(memPerWF)/(8*1024*1024*1024*0.8))
	fmt.Println()

	fmt.Println("  Bottleneck 4: Event history reads (replay)")
	fmt.Println("  ────────────────────────────────────────────")
	fmt.Println("    Replay only happens on worker crash/restart.")
	fmt.Println("    A workflow with 100 steps replays in ~1-10ms")
	fmt.Println("    (indexed B-tree scan on workflow_id, step).")
	fmt.Println("    At 1%% crash rate and 10K concurrent workflows:")
	fmt.Println("    ~100 replays/sec × ~10ms = negligible DB load.")
	fmt.Println()
}

func hardLimits() {
	fmt.Println("── 4. HARD LIMITS ──")
	fmt.Println()
	fmt.Println("  The architecture hits hard limits at different scales:")
	fmt.Println()

	limits := []struct {
		component string
		limit     string
		atScale   string
		mitigation string
	}{
		{
			"PostgreSQL writes",
			"~100K INSERTs/sec (single instance, high-end HW)",
			"~12.5K wf completions/sec (at 8 steps/wf)",
			"Partition event_history by hash; multiple PG instances (sharding)",
		},
		{
			"PostgreSQL connections",
			"~500-1000 active connections before context-switch overhead dominates",
			"~200-500 workers with pooled connections",
			"PgBouncer transaction pooling; 1000 workers → ~50 PG connections",
		},
		{
			"Worker memory",
			"~3 MB per concurrent workflow",
			"100 workers × 8 GB = ~200K concurrent workflows",
			"Add more workers (stateless, horizontally scalable)",
		},
		{
			"WASM module cache",
			"~200 KB per version; thousands of versions trivially cached",
			"10K workflow versions × 200 KB = 2 GB (fits in worker RAM)",
			"LRU eviction; rarely-used versions loaded from DB on demand",
		},
		{
			"Network bandwidth",
			"Workers → external APIs: depends on response sizes",
			"1000 calls/sec × 10KB responses = 10 MB/sec (negligible)",
			"Not a bottleneck for typical JSON API patterns",
		},
		{
			"Patroni failover time",
			"~30-60 seconds (etcd TTL + promote + reconnect)",
			"During failover, all workers pause. Heartbeat timeout must exceed failover time.",
			"Set heartbeat timeout to 90s (comfortably > 60s failover)",
		},
	}

	fmt.Println("  ┌─────────────────────┬──────────────────────────────────┬──────────────────────┐")
	fmt.Println("  │ Component           │ Hard limit                       │ Mitigation           │")
	fmt.Println("  ├─────────────────────┼──────────────────────────────────┼──────────────────────┤")
	for _, l := range limits {
		fmt.Printf("  │ %-19s │ %-32s │ %-20s │\n",
			l.component, l.limit, l.mitigation)
	}
	fmt.Println("  └─────────────────────┴──────────────────────────────────┴──────────────────────┘")
	fmt.Println()
	fmt.Println("  The fundamental scaling ceiling is PostgreSQL write throughput.")
	fmt.Println("  Everything else (workers, WASM loading, network) scales linearly")
	fmt.Println("  by adding more instances. PostgreSQL can scale vertically to")
	fmt.Println("  ~100K writes/sec, then requires sharding.")
	fmt.Println()
	fmt.Println("  For comparison: at 100K writes/sec with 8-step workflows, that's")
	fmt.Println("  12,500 workflow completions per second, or over 1 BILLION per day.")
	fmt.Println("  This is far beyond what most business workflow systems need.")
	fmt.Println()
}

func stagedScaling() {
	fmt.Println("── 5. STAGED SCALING PLAN ──")
	fmt.Println()

	stages := []struct {
		name        string
		trigger     string
		changes     []string
		newCap      string
		costDelta   string
	}{
		{
			"Stage 1: Single PG",
			"Starting point",
			[]string{
				"1 PostgreSQL instance (db.r6g.large)",
				"3 workers (t3.large)",
				"Event history in single table",
				"WASM blobs in BYTEA column",
				"SKIP LOCKED for queue",
			},
			"~3K concurrent, ~500 steps/sec",
			"~$350/month",
		},
		{
			"Stage 2: Vertical PG",
			"Event history > 50 GB or > 1K INSERTs/sec sustained",
			[]string{
				"Upgrade PG to db.r6g.2xlarge (8 vCPU, 64 GB)",
				"Add read replica for dashboards/queries",
				"Add 10 workers",
				"WASM blobs to S3 (BYTEA → URL reference)",
			},
			"~30K concurrent, ~5K steps/sec",
			"~$1,200/month",
		},
		{
			"Stage 3: Partitioning",
			"Event history > 500 GB or > 5K INSERTs/sec sustained",
			[]string{
				"Partition event_history by hash(workflow_id)",
				"PARTITION BY HASH (workflow_id) PARTITIONS 16",
				"Each partition independently vacuumed",
				"Add 30 workers",
			},
			"~60K concurrent, ~10K steps/sec",
			"~$3,000/month",
		},
		{
			"Stage 4: Split queue",
			"SKIP LOCKED claim latency > 10ms p99",
			[]string{
				"Add Redis for task queue (XREADGROUP + XCLAIM)",
				"PostgreSQL remains state-of-record",
				"Write to PG first, ACK Redis second",
				"Add 100 workers",
			},
			"~200K concurrent, ~20K steps/sec",
			"~$8,000/month",
		},
		{
			"Stage 5: Sharded state",
			"PostgreSQL write throughput saturated",
			[]string{
				"Shard by workflow_id across multiple PG instances",
				"Alternative: migrate to FoundationDB or CockroachDB",
				"Host-level connection routing",
				"Cross-shard workflows rare (most are single-shard)",
			},
			"~1M concurrent, ~100K steps/sec",
			"~$25,000+/month",
		},
	}

	fmt.Println("  ┌──────────┬───────────────┬──────────────────────────────────┬─────────────┐")
	fmt.Println("  │ Stage    │ Trigger        │ Changes                          │ Cost/mo     │")
	fmt.Println("  ├──────────┼───────────────┼──────────────────────────────────┼─────────────┤")
	for _, s := range stages {
		fmt.Printf("  │ %-8s │ %-13s │ %-32s │ %-11s │\n",
			s.name, s.trigger, s.changes[0], s.costDelta)
		for i := 1; i < len(s.changes); i++ {
			fmt.Printf("  │          │               │ %-32s │             │\n", s.changes[i])
		}
		fmt.Printf("  │          │               │ → Capacity: %-21s │             │\n", s.newCap)
		fmt.Println("  ├──────────┼───────────────┼──────────────────────────────────┼─────────────┤")
	}
	fmt.Println("  └──────────┴───────────────┴──────────────────────────────────┴─────────────┘")
	fmt.Println()
	fmt.Println("  The key property: each stage is incremental. You don't need to")
	fmt.Println("  design for Stage 5 on day one. The data model (workflow_id, step")
	fmt.Println("  for event history; workflow_id for queue) is partitionable from")
	fmt.Println("  the start — so you CAN shard later without rewriting everything.")
	fmt.Println()
}

func comparisonToTemporal() {
	fmt.Println("── 6. COST COMPARISON: THIS SYSTEM vs TEMPORAL ──")
	fmt.Println()
	fmt.Println("  Temporal's operational cost comes from running 4+ services")
	fmt.Println("  (Frontend, History, Matching, Worker) plus a persistence")
	fmt.Println("  layer. This system collapses to PostgreSQL + workers.")
	fmt.Println()
	fmt.Println("  Approximate comparison for 'Medium Production' (~10K concurrent):")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────┬──────────────────┬──────────────────┐")
	fmt.Println("  │ Resource                   │ This system      │ Temporal         │")
	fmt.Println("  ├────────────────────────────┼──────────────────┼──────────────────┤")
	fmt.Println("  │ Database (state + queue)   │ 1 PG (4 vCPU)    │ 1 DB + 3 services│")
	fmt.Println("  │                            │ $360/mo          │ $???/mo          │")
	fmt.Println("  │ Workers                    │ 10 × t3.large    │ 10 × t3.large    │")
	fmt.Println("  │                            │ $600/mo          │ $600/mo          │")
	fmt.Println("  │ Temporal services          │ N/A              │ Frontend $180/mo  │")
	fmt.Println("  │                            │                  │ History $360/mo   │")
	fmt.Println("  │                            │                  │ Matching $180/mo  │")
	fmt.Println("  │ Storage                    │ $40/mo (100 GB)  │ $40/mo (100 GB)  │")
	fmt.Println("  ├────────────────────────────┼──────────────────┼──────────────────┤")
	fmt.Println("  │ TOTAL                      │ ~$1,010/mo       │ ~$2,360/mo★      │")
	fmt.Println("  └────────────────────────────┴──────────────────┴──────────────────┘")
	fmt.Println("  ★ Temporal estimate: 3 AZs × small instances for each service,")
	fmt.Println("    plus persistence DB. Actual costs vary significantly.")
	fmt.Println()
	fmt.Println("  This is a rough estimate. Temporal's actual cost depends on")
	fmt.Println("  scale, configuration, and whether you use Temporal Cloud.")
	fmt.Println("  The structural advantage is real: fewer services = fewer")
	fmt.Println("  instances = lower baseline cost. But Temporal Cloud abstracts")
	fmt.Println("  this away — you pay per workflow execution, not per service.")
	fmt.Println()
	fmt.Println("  Temporal Cloud pricing (~$0.025/1K workflow steps) vs this")
	fmt.Println("  system's fixed infrastructure cost means the crossover point")
	fmt.Println("  depends on volume. At low volume, Temporal Cloud is cheaper")
	fmt.Println("  (no idle infrastructure). At high volume, fixed infrastructure")
	fmt.Println("  wins. The crossover is roughly:")
	fmt.Println()
	fmt.Println("    Temporal Cloud: $0.025 per 1,000 steps")
	fmt.Println("    This system:     $1,000/month fixed")
	fmt.Println("    Crossover:       $1,000 / $0.000025 = 40M steps/month")
	fmt.Println("                     ≈ 13 steps/second sustained")
	fmt.Println()
	fmt.Println("  Below 40M steps/month: Temporal Cloud is cheaper.")
	fmt.Println("  Above 40M steps/month: Fixed infrastructure wins.")
	fmt.Println("  But volume isn't the only factor — operational simplicity,")
	fmt.Println("  built-in observability, and versioning model are the main")
	fmt.Println("  differentiators, not cost.")
	fmt.Println()
}

func _unused() {
	// Suppress "unused const" warnings — these are referenced in the
	// printf-heavy analysis but Go doesn't see them in all code paths.
	_ = fmt.Sprintf("")
}
