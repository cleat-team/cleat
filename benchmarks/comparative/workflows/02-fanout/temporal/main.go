// Temporal Go SDK implementation of FanOutWorkflow.
//
// Mirrors the Cleat FanOutWorkflow in benchmarks/workflows/fanout.go:
// N child workflows are spawned and awaited. The parent workflow starts
// each child concurrently and collects all results. Each child performs
// a single activity (noop) and returns.
//
// Usage:
//
//	# Start Temporal dev server first:
//	temporal server start-dev --db-file /tmp/temporal-bench.db &
//
//	# Run benchmarks:
//	go run main.go -warmup=10s -benchtime=60s -concurrency=4
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
// Types — mirrors benchmarks/workflows/fanout.go
// ---------------------------------------------------------------------------

// FanOutInput configures the number of child workflows to spawn.
type FanOutInput struct {
	Children int `json:"children"`
}

// FanOutOutput is the result of the fan-out workflow.
type FanOutOutput struct {
	Completed int `json:"completed"`
}

// NoopChildOutput is the result of the noop child workflow.
type NoopChildOutput struct {
	Status string `json:"status"`
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// NoopActivity is the per-child activity — equivalent to a DurableCall.
func NoopActivity(ctx context.Context) (string, error) {
	return `{"status":"ok"}`, nil
}

// ---------------------------------------------------------------------------
// Child workflow
// ---------------------------------------------------------------------------

// NoopChildWorkflow executes a single noop activity and returns.
// This mirrors the NoopChild function in benchmarks/workflows/fanout.go.
func NoopChildWorkflow(ctx workflow.Context) (NoopChildOutput, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    5 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Second,
	})
	var result string
	if err := workflow.ExecuteActivity(ctx, "noopActivity").Get(ctx, &result); err != nil {
		return NoopChildOutput{}, fmt.Errorf("child noop activity: %w", err)
	}
	return NoopChildOutput{Status: "ok"}, nil
}

// ---------------------------------------------------------------------------
// Parent workflow
// ---------------------------------------------------------------------------

// FanOutWorkflow spawns N child workflows and waits for all to complete.
// This mirrors the Cleat FanOutWorkflow which calls ChildWorkflow for each
// child and then AwaitAllChildren.
func FanOutWorkflow(ctx workflow.Context, input FanOutInput) (FanOutOutput, error) {
	var futures []workflow.ChildWorkflowFuture
	for i := 0; i < input.Children; i++ {
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:          fmt.Sprintf("child-%d", i),
			WorkflowTaskTimeout: 10 * time.Second,
		})
		future := workflow.ExecuteChildWorkflow(childCtx, "noopChildWorkflow")
		futures = append(futures, future)
	}

	completed := 0
	for _, future := range futures {
		var result NoopChildOutput
		if err := future.Get(ctx, &result); err != nil {
			log.Printf("Child workflow failed: %v", err)
			continue
		}
		if result.Status == "ok" {
			completed++
		}
	}
	return FanOutOutput{Completed: completed}, nil
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

// runBenchmark executes warm-up + measurement phases with concurrent
// workflow executions.
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

	executeOne := func() {
		id := atomic.AddInt64(&idSeq, 1)
		wfID := fmt.Sprintf("bench-%s-%d", workflowName, id)
		opts := client.StartWorkflowOptions{
			ID:                  wfID,
			TaskQueue:           cfg.TaskQueue,
			WorkflowTaskTimeout: 30 * time.Second,
		}
		run, err := c.ExecuteWorkflow(context.Background(), opts, wfFn, input)
		if err != nil {
			return
		}
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
		Config:      fmt.Sprintf("children=%d", input.(*FanOutInput).Children),
		Count:       count,
		Elapsed:     elapsed,
		WFPerSec:    wfPerSec,
		StepsPerSec: stepsPerSec,
	}
}

func printResult(r BenchmarkResult) {
	fmt.Printf("\nBenchmark%s/config=%s  count=%d  %.0f ns/wf  %.2f wf/s  %.2f steps/s\n",
		r.Name, r.Config, r.Count,
		float64(r.Elapsed.Nanoseconds())/float64(r.Count),
		r.WFPerSec, r.StepsPerSec)
	fmt.Printf("BENCHMARK_RESULT  name=%s  config=%s  count=%d  elapsed=%.3fs  wf_per_sec=%.2f  steps_per_sec=%.2f\n",
		r.Name, r.Config, r.Count, r.Elapsed.Seconds(), r.WFPerSec, r.StepsPerSec)
}

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
	concurrency := flag.Int("concurrency", 4, "Number of concurrent parent workflow executions")
	address := flag.String("address", "localhost:7233", "Temporal server address (host:port)")
	flag.Parse()

	c := mustDial(*address)
	defer c.Close()

	taskQueue := "benchmark-fanout"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(FanOutWorkflow)
	w.RegisterWorkflow(NoopChildWorkflow)
	w.RegisterActivity(NoopActivity)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() {
		if err := w.Run(workerCtx); err != nil {
			log.Fatalf("FATAL: worker exited: %v", err)
		}
	}()

	time.Sleep(2 * time.Second)

	cfg := BenchmarkConfig{
		Warmup:      *warmup,
		Benchtime:   *benchtime,
		Concurrency: *concurrency,
		TaskQueue:   taskQueue,
	}

	// Test cases matching benchmarks/cleat_bench_test.go.
	testCases := []int{10, 100, 500}
	for _, children := range testCases {
		fmt.Printf("\n========== FanOutWorkflow children=%d ==========\n", children)
		input := &FanOutInput{Children: children}

		// Steps per workflow: children ChildWorkflow calls + children child
		// Activity calls + children child result collection = 3*children.
		stepsPerWF := 3 * children
		result := runBenchmark(c, cfg, FanOutWorkflow, input, stepsPerWF, "FanOutWorkflow")
		printResult(result)
	}
}
