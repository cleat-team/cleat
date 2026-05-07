// Temporal Go SDK implementation of SimpleWorkflow.
//
// Mirrors the Cleat SimpleWorkflow in benchmarks/workflows/simple.go:
// N sequential Activity.Execute calls that go through the full Temporal
// plumbing (client -> worker -> activity execution), measuring end-to-end
// framework overhead per step.
//
// Usage:
//
//	# Start Temporal dev server first:
//	temporal server start-dev --db-file /tmp/temporal-bench.db &
//
//	# Run benchmarks:
//	go run main.go -warmup=10s -benchtime=60s -concurrency=10
//
//	# With custom address:
//	go run main.go -address=localhost:7233 -warmup=10s -benchtime=60s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// ---------------------------------------------------------------------------
// Types — mirrors benchmarks/workflows/simple.go
// ---------------------------------------------------------------------------

// SimpleInput configures the number of sequential steps.
type SimpleInput struct {
	Steps int `json:"steps"`
}

// SimpleOutput is the result of the simple workflow.
type SimpleOutput struct {
	Done bool `json:"done"`
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// NoopActivity is the Temporal equivalent of h.DurableCall("bench","noop","{}").
// It returns immediately with a success payload.
func NoopActivity(ctx context.Context) (string, error) {
	return `{"status":"ok"}`, nil
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

// SimpleWorkflow executes N sequential activity calls.  Each call is a
// separate Temporal ActivityExecution, which exercises the full activity
// dispatch path (marshalling, scheduling, worker pick-up, execution, result
// return).
func SimpleWorkflow(ctx workflow.Context, input SimpleInput) (SimpleOutput, error) {
	// Activity options: short timeout since we're benchmarking.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Second,
		HeartbeatTimeout:    0, // not needed for synchronous activities
	})

	for i := 0; i < input.Steps; i++ {
		var result string
		if err := workflow.ExecuteActivity(ctx, "noopActivity").Get(ctx, &result); err != nil {
			return SimpleOutput{}, fmt.Errorf("step %d: %w", i, err)
		}
	}
	return SimpleOutput{Done: true}, nil
}

// ---------------------------------------------------------------------------
// Benchmark infrastructure
// ---------------------------------------------------------------------------

// BenchmarkConfig holds parameters for a single benchmark run.
type BenchmarkConfig struct {
	Warmup      time.Duration
	Benchtime   time.Duration
	Concurrency int
	TaskQueue   string
}

// BenchmarkResult holds the measured outcome of a benchmark run.
type BenchmarkResult struct {
	Name        string
	Config      string
	Count       int64
	Elapsed     time.Duration
	WFPerSec    float64
	StepsPerSec float64
}

// runBenchmark executes a warm-up phase followed by a measurement phase.
// It spawns cfg.Concurrency goroutines, each executing workflows as fast as
// possible against the Temporal server.  All goroutines share one Temporal
// client.
func runBenchmark(
	c client.Client,
	cfg BenchmarkConfig,
	wfFn interface{},
	input interface{},
	stepsPerWorkflow int,
	workflowName string,
) BenchmarkResult {
	var count int64
	var idSeq int64

	// executeOne starts one workflow and waits for completion.
	executeOne := func() {
		id := atomic.AddInt64(&idSeq, 1)
		wfID := fmt.Sprintf("bench-%s-%d", workflowName, id)
		opts := client.StartWorkflowOptions{
			ID:                 wfID,
			TaskQueue:          cfg.TaskQueue,
			WorkflowTaskTimeout: 10 * time.Second,
		}
		run, err := c.ExecuteWorkflow(context.Background(), opts, wfFn, input)
		if err != nil {
			return
		}
		// Wait for completion so we don't overload the server with pending
		// executions (which would change the measurement).
		var result interface{}
		_ = run.Get(context.Background(), &result)
		atomic.AddInt64(&count, 1)
	}

	// ---- Warm-up ----
	fmt.Fprintf(log.Writer(), "[warmup] running for %v with concurrency=%d ...\n", cfg.Warmup, cfg.Concurrency)
	warmupEnd := time.Now().Add(cfg.Warmup)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(warmupEnd) {
				executeOne()
			}
		}()
	}
	wg.Wait()
	fmt.Fprintf(log.Writer(), "[warmup] done\n")

	// ---- Measurement ----
	count = 0
	fmt.Fprintf(log.Writer(), "[measure] running for %v with concurrency=%d ...\n", cfg.Benchtime, cfg.Concurrency)
	measureStart := time.Now()
	measureEnd := measureStart.Add(cfg.Benchtime)

	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(measureEnd) {
				executeOne()
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(measureStart)
	if elapsed <= 0 {
		elapsed = 1
	}
	wfPerSec := float64(count) / elapsed.Seconds()
	stepsPerSec := wfPerSec * float64(stepsPerWorkflow)

	return BenchmarkResult{
		Name:        workflowName,
		Config:      fmt.Sprintf("steps=%d", input.(*SimpleInput).Steps),
		Count:       count,
		Elapsed:     elapsed,
		WFPerSec:    wfPerSec,
		StepsPerSec: stepsPerSec,
	}
}

// printResult outputs the benchmark result in a machine-parseable format
// similar to Go's testing.B output.
func printResult(r BenchmarkResult) {
	fmt.Printf("\nBenchmark%s/config=%s  count=%d  %.0f ns/wf  %.2f wf/s  %.2f steps/s\n",
		r.Name, r.Config, r.Count,
		float64(r.Elapsed.Nanoseconds())/float64(r.Count),
		r.WFPerSec, r.StepsPerSec)
	fmt.Printf("BENCHMARK_RESULT  name=%s  config=%s  count=%d  elapsed=%.3fs  wf_per_sec=%.2f  steps_per_sec=%.2f\n",
		r.Name, r.Config, r.Count, r.Elapsed.Seconds(), r.WFPerSec, r.StepsPerSec)
}

// mustDial connects to the Temporal server and returns a client, or exits.
func mustDial(address string) client.Client {
	c, err := client.Dial(client.Options{
		HostPort:  address,
		Namespace: "default",
	})
	if err != nil {
		log.Fatalf("FATAL: unable to dial Temporal at %s: %v", address, err)
	}
	return c
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	warmup := flag.Duration("warmup", 10*time.Second, "Warm-up phase duration")
	benchtime := flag.Duration("benchtime", 60*time.Second, "Measurement phase duration")
	concurrency := flag.Int("concurrency", 10, "Number of concurrent workflow executions")
	address := flag.String("address", "localhost:7233", "Temporal server address (host:port)")
	flag.Parse()

	// Connect, worker, client
	c := mustDial(*address)
	defer c.Close()

	taskQueue := "benchmark-simple"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(SimpleWorkflow)
	w.RegisterActivity(NoopActivity)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() {
		if err := w.Run(workerCtx); err != nil {
			log.Fatalf("FATAL: worker exited: %v", err)
		}
	}()

	// Give the worker a moment to register with the server.
	time.Sleep(2 * time.Second)

	cfg := BenchmarkConfig{
		Warmup:      *warmup,
		Benchtime:   *benchtime,
		Concurrency: *concurrency,
		TaskQueue:   taskQueue,
	}

	// Test cases matching benchmarks/cleat_bench_test.go.
	testCases := []int{10, 100, 1000}
	for _, steps := range testCases {
		fmt.Printf("\n========== SimpleWorkflow steps=%d ==========\n", steps)
		input := &SimpleInput{Steps: steps}
		result := runBenchmark(c, cfg, SimpleWorkflow, input, steps, "SimpleWorkflow")
		printResult(result)
	}
}
