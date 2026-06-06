// Command cleat-bench is a performance benchmark tool for cleat workers.
// It measures throughput, latency, and replay performance against a real
// PostgreSQL database.
//
// Usage:
//
//	cleat-bench --db "postgres://..." --workflow <name> --count 100
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine"
)

func main() {
	dbURL := flag.String("db", "", "Database connection URL (required, or set DATABASE_URL)")
	workflowName := flag.String("workflow", "", "Workflow definition name to benchmark")
	entryPoint := flag.String("entry-point", "place_order", "Workflow entry point")
	count := flag.Int("count", 100, "Number of workflow executions")
	concurrency := flag.Int("concurrency", 10, "Max concurrent executions")
	taskQueueStr := flag.String("task-queue", "default", "Task queue to poll (e.g. default, gpu, high-memory)")
	driver := flag.String("driver", "postgres", "Database driver: postgres, mysql, or mssql")
	flag.Parse()

	if *dbURL == "" {
		*dbURL = os.Getenv("DATABASE_URL")
	}
	if *dbURL == "" || *workflowName == "" {
		fmt.Fprintf(os.Stderr, "Usage: cleat-bench --db <url> --workflow <name> [--driver postgres] [--count 100] [--concurrency 10]\n")
		os.Exit(1)
	}

	var sqlDriver string
	switch *driver {
	case "postgres":
		sqlDriver = "postgres"
	case "mysql":
		sqlDriver = "mysql"
	case "mssql":
		sqlDriver = "mssql"
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid --driver %q; must be postgres, mysql, or mssql\n", *driver)
		os.Exit(1)
	}

	const tenantID = "00000000-0000-0000-0000-000000000000"

	ctx := context.Background()
	taskQueues := strings.Split(*taskQueueStr, ",")

	var factory engine.StoreFactory
	switch *driver {
	case "postgres":
		db, err := sql.Open(sqlDriver, *dbURL)
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(*concurrency + 10)
		db.SetMaxIdleConns(10)
		factory = engine.NewPostgresStoreFactory(db, "public")
	case "mysql":
		db, err := sql.Open(sqlDriver, *dbURL)
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		defer db.Close()
		db.SetMaxOpenConns(*concurrency + 10)
		db.SetMaxIdleConns(10)
		factory = engine.NewMySQLStoreFactory(db, *dbURL)
	case "mssql":
		factory = engine.NewMSSQLStoreFactory(*dbURL)
	}
	store, closer, err := factory.OpenStore(ctx, tenantID, taskQueues...)
	if err != nil {
		log.Fatalf("Failed to open store: %v", err)
	}
	defer closer.Close()

	// Find the latest version of the workflow.
	versions, err := store.ListVersions(ctx, *workflowName)
	if err != nil {
		log.Fatalf("Failed to list versions: %v", err)
	}
	if len(versions) == 0 {
		log.Fatalf("Workflow %q not found. Deploy it first with: cleat deploy <wasm-file>", *workflowName)
	}
	version := versions[0]

	fmt.Printf("Benchmark: %s v%d, %d executions, %d concurrent\n",
		*workflowName, version, *count, *concurrency)

	// ---- Fresh execution benchmark ----
	fmt.Println("\n=== Fresh Execution ===")
	freshLatencies := runBenchmark(ctx, store, *workflowName, version, *entryPoint, *count, *concurrency)
	reportStats("fresh", freshLatencies)

	// ---- Replay benchmark ----
	fmt.Println("\n=== Replay ===")
	replayLatencies := runReplayBenchmark(ctx, store, *workflowName, version, *entryPoint, *count, *concurrency)
	reportStats("replay", replayLatencies)
}

func runBenchmark(ctx context.Context, store engine.WorkflowStore, defName string, defVersion int, entryPoint string, count, concurrency int) []time.Duration {
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var latenciesMu sync.Mutex
	var latencies []time.Duration
	var completed int64

	input := json.RawMessage(fmt.Sprintf(`{"__entry_point":"%s","order_id":"bench"}`, entryPoint))
	wasmBytes, _ := store.LoadWASM(ctx, defName, defVersion)

	for i := 0; i < count; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()

			runID, _, err := store.StartNewRun(ctx, "", defName, defVersion, input, "", engine.DefaultTenantUUID, 0)
			if err != nil {
				log.Printf("StartNewRun error: %v", err)
				return
			}

			rt, err := engine.NewRuntime(ctx, 0, 0)
			if err != nil {
				log.Printf("NewRuntime error: %v", err)
				return
			}
			defer rt.Close(ctx)

			eng := engine.NewEngine(rt, &benchCaller{},
				engine.WithSignalStore(store),
				engine.WithWorkflowState(&benchState{version: defVersion}),
				engine.WithWorkflowID(runID),
			)

			result, history, _, _, _, err := eng.Execute(ctx, wasmBytes, entryPoint, input)
			if err != nil {
				log.Printf("Execute error for %s: %v", runID, err)
				store.FailWorkflow(ctx, runID, "", 0, err.Error(), "", "", nil)
				return
			}

			_ = result
			_ = history

			store.CompleteWorkflow(ctx, runID, "", 0, "{}", nil)

			elapsed := time.Since(start)
			latenciesMu.Lock()
			latencies = append(latencies, elapsed)
			latenciesMu.Unlock()

			n := atomic.AddInt64(&completed, 1)
			if n%10 == 0 {
				fmt.Printf("  %d/%d completed\n", n, count)
			}
		}()
	}
	wg.Wait()
	return latencies
}

func runReplayBenchmark(ctx context.Context, store engine.WorkflowStore, defName string, defVersion int, entryPoint string, count, concurrency int) []time.Duration {
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var latenciesMu sync.Mutex
	var latencies []time.Duration
	var completed int64

	input := json.RawMessage(fmt.Sprintf(`{"__entry_point":"%s","order_id":"bench-replay"}`, entryPoint))
	wasmBytes, _ := store.LoadWASM(ctx, defName, defVersion)

	for i := 0; i < count; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()

			runID, _, err := store.StartNewRun(ctx, "", defName, defVersion, input, "", engine.DefaultTenantUUID, 0)
			if err != nil {
				log.Printf("StartNewRun error: %v", err)
				return
			}

			rt, err := engine.NewRuntime(ctx, 0, 0)
			if err != nil {
				log.Printf("NewRuntime error: %v", err)
				return
			}
			defer rt.Close(ctx)

			caller := &benchCaller{}
			eng := engine.NewEngine(rt, caller,
				engine.WithSignalStore(store),
				engine.WithWorkflowState(&benchState{version: defVersion}),
				engine.WithWorkflowID(runID),
			)

			// First execution.
			_, history, _, _, _, err := eng.Execute(ctx, wasmBytes, entryPoint, input)
			if err != nil {
				log.Printf("First execute error: %v", err)
				store.FailWorkflow(ctx, runID, "", 0, err.Error(), "", "", nil)
				return
			}

			// Save events.
			store.AppendEventHistoryBatch(ctx, runID, history)

			// Replay from history.
			rt2, _ := engine.NewRuntime(ctx, 0, 0)
			defer rt2.Close(ctx)
			engine2 := engine.NewEngine(rt2, caller,
				engine.WithSignalStore(store),
				engine.WithWorkflowState(&benchState{version: defVersion}),
				engine.WithWorkflowID(runID),
			)

			_, _, _, _, _, err = engine2.Replay(ctx, wasmBytes, entryPoint, input, history)
			if err != nil {
				log.Printf("Replay error: %v", err)
				store.FailWorkflow(ctx, runID, "", 0, err.Error(), "", "", nil)
				return
			}

			store.CompleteWorkflow(ctx, runID, "", 0, "{}", nil)

			elapsed := time.Since(start)
			latenciesMu.Lock()
			latencies = append(latencies, elapsed)
			latenciesMu.Unlock()

			n := atomic.AddInt64(&completed, 1)
			if n%10 == 0 {
				fmt.Printf("  %d/%d completed\n", n, count)
			}
		}()
	}
	wg.Wait()
	return latencies
}

func reportStats(label string, latencies []time.Duration) {
	if len(latencies) == 0 {
		fmt.Printf("%s: no data\n", label)
		return
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	avg := sum / time.Duration(len(latencies))
	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	p99 := latencies[len(latencies)*99/100]

	throughput := float64(len(latencies)) / sum.Seconds()

	fmt.Printf("%s: count=%d throughput=%.1f/s avg=%v p50=%v p95=%v p99=%v\n",
		label, len(latencies), throughput, avg, p50, p95, p99)
}

// -- Benchmark helpers --

type benchCaller struct{}

func (c *benchCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	// Simulate a fast external call.
	return `{"status":"ok"}`, nil
}

type benchState struct {
	version    int
	minVersion int
}

func (s *benchState) Version() int                  { return s.version }
func (s *benchState) MinVersion() int               { return s.minVersion }
func (s *benchState) Priority() int                     { return 0 }
func (s *benchState) ChildVersion(name string) (int, bool) { return 0, false }

func init() {
	_ = math.Sqrt // force math import usage
}
