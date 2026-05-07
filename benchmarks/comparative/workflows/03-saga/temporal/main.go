// Temporal Go SDK implementation of SagaWorkflow and
// SagaWithCompensationWorkflow.
//
// Mirrors the Cleat saga implementations in benchmarks/workflows/saga.go:
// N steps with forward and compensation actions.  Two variants:
//   - SagaWorkflow (happy path): all steps succeed, no compensation.
//   - SagaWithCompensationWorkflow: one step fails, all previously
//     completed steps are compensated in reverse order.
//
// Temporal removed its built-in Saga helper, so we implement the
// compensation pattern manually via a slice of compensation closures.
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
// Types — mirrors benchmarks/workflows/saga.go
// ---------------------------------------------------------------------------

// SagaInput configures the happy-path saga.
type SagaInput struct {
	Steps int `json:"steps"`
}

// SagaOutput for the happy path.
type SagaOutput struct {
	Done bool `json:"done"`
}

// SagaWithCompensationInput configures the saga-with-failure benchmark.
type SagaWithCompensationInput struct {
	Steps      int `json:"steps"`
	FailAtStep int `json:"fail_at_step"`
}

// SagaWithCompensationOutput is the result of the saga-with-failure workflow.
type SagaWithCompensationOutput struct {
	Compensated int  `json:"compensated"`
	Failed      bool `json:"failed"`
}

// ---------------------------------------------------------------------------
// Activities
// ---------------------------------------------------------------------------

// ForwardActivity simulates a successful step action.
func ForwardActivity(ctx context.Context) (string, error) {
	return `{"status":"ok"}`, nil
}

// CompensateActivity simulates a compensation action.
func CompensateActivity(ctx context.Context) (string, error) {
	return `{"status":"compensated"}`, nil
}

// FailingForwardActivity simulates a step that always fails — used for the
// failure-at-step-N pattern.  The activity itself doesn't know which step
// it is; the workflow decides whether to fail based on the step index.
// However, for the Temporal model, the activity logic is simple: it always
// succeeds.  The workflow itself triggers the failure by returning an error
// when the step index matches FailAtStep.
func FailingForwardActivity(ctx context.Context) (string, error) {
	return `{"status":"ok"}`, nil
}

// ---------------------------------------------------------------------------
// SagaWorkflow — happy path (all steps succeed)
// ---------------------------------------------------------------------------

// SagaWorkflow executes N steps with compensation handlers registered for each.
// All steps succeed, so compensation is never triggered.  This benchmarks the
// overhead of the compensation scaffolding (closure registration, loop
// overhead, activity dispatch).
func SagaWorkflow(ctx workflow.Context, input SagaInput) (SagaOutput, error) {
	type step struct {
		name    string
		compensate func(workflow.Context) error
	}

	var steps []step

	for i := 0; i < input.Steps; i++ {
		i := i // capture
		actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout:    5 * time.Second,
			ScheduleToCloseTimeout: 30 * time.Second,
		})

		var result string
		if err := workflow.ExecuteActivity(actCtx, "forwardActivity").Get(actCtx, &result); err != nil {
			return SagaOutput{}, fmt.Errorf("step %d forward: %w", i, err)
		}

		steps = append(steps, step{
			name: fmt.Sprintf("step_%d", i),
			compensate: func(ctx workflow.Context) error {
				compCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
					StartToCloseTimeout:    5 * time.Second,
					ScheduleToCloseTimeout: 30 * time.Second,
				})
				var compResult string
				return workflow.ExecuteActivity(compCtx, "compensateActivity").Get(compCtx, &compResult)
			},
		})
	}

	return SagaOutput{Done: true}, nil
}

// ---------------------------------------------------------------------------
// SagaWithCompensationWorkflow — failure path
// ---------------------------------------------------------------------------

// SagaWithCompensationWorkflow runs a saga where one step fails, triggering
// compensation of all previously completed steps.  If FailAtStep equals
// the current step index, the workflow returns an error (simulating step
// failure), and the compensation chain runs in reverse order.
func SagaWithCompensationWorkflow(ctx workflow.Context, input SagaWithCompensationInput) (SagaWithCompensationOutput, error) {
	type compensationFn func(workflow.Context) error
	var compensations []compensationFn

	for i := 0; i < input.Steps; i++ {
		i := i // capture
		actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout:    5 * time.Second,
			ScheduleToCloseTimeout: 30 * time.Second,
		})

		// Simulate the Cleat behaviour: forward action returns an error
		// when idx == FailAtStep.
		if i == input.FailAtStep {
			// Record that we made it here; the step fails.
			// Compensate all previously registered steps in reverse order.
			for j := len(compensations) - 1; j >= 0; j-- {
				_ = compensations[j](ctx) // best-effort compensation
			}
			return SagaWithCompensationOutput{
				Compensated: input.FailAtStep,
				Failed:      true,
			}, fmt.Errorf("simulated failure at step %d", i)
		}

		var result string
		if err := workflow.ExecuteActivity(actCtx, "forwardActivity").Get(actCtx, &result); err != nil {
			// Unexpected activity failure; attempt compensation.
			for j := len(compensations) - 1; j >= 0; j-- {
				_ = compensations[j](ctx)
			}
			return SagaWithCompensationOutput{
				Compensated: i,
				Failed:      true,
			}, fmt.Errorf("step %d: unexpected activity failure: %w", i, err)
		}

		// Register compensation for this step.
		compensations = append(compensations, func(ctx workflow.Context) error {
			compCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout:    5 * time.Second,
				ScheduleToCloseTimeout: 30 * time.Second,
			})
			var compResult string
			return workflow.ExecuteActivity(compCtx, "compensateActivity").Get(compCtx, &compResult)
		})
	}

	return SagaWithCompensationOutput{Compensated: 0, Failed: false}, nil
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

// runBenchmark executes warm-up + measurement phases.
func runBenchmark(
	c client.Client,
	cfg BenchmarkConfig,
	wfFn interface{},
	input interface{},
	stepsPerWorkflow int,
	workflowName string,
	configLabel string,
) BenchmarkResult {
	var count int64
	var idSeq int64

	executeOne := func() {
		id := atomic.AddInt64(&idSeq, 1)
		wfID := fmt.Sprintf("bench-%s-%d", workflowName, id)
		opts := client.StartWorkflowOptions{
			ID:                 wfID,
			TaskQueue:          cfg.TaskQueue,
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
		Config:      configLabel,
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
	concurrency := flag.Int("concurrency", 10, "Number of concurrent workflow executions")
	address := flag.String("address", "localhost:7233", "Temporal server address (host:port)")
	flag.Parse()

	c := mustDial(*address)
	defer c.Close()

	taskQueue := "benchmark-saga"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(SagaWorkflow)
	w.RegisterWorkflow(SagaWithCompensationWorkflow)
	w.RegisterActivity(ForwardActivity)
	w.RegisterActivity(CompensateActivity)
	w.RegisterActivity(FailingForwardActivity)

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

	// ---- Happy path (SagaWorkflow) ----
	happyCases := []int{10, 100, 1000}
	for _, steps := range happyCases {
		fmt.Printf("\n========== SagaWorkflow steps=%d ==========\n", steps)
		input := &SagaInput{Steps: steps}
		// steps per workflow: one forward activity per step.
		result := runBenchmark(c, cfg, SagaWorkflow, input, steps, "SagaWorkflow",
			fmt.Sprintf("steps=%d", steps))
		printResult(result)
	}

	// ---- Failure path (SagaWithCompensationWorkflow) ----
	failureCases := []struct {
		steps int
		failAt int
	}{
		{steps: 10, failAt: 9},   // last step fails
		{steps: 100, failAt: 99}, // last step fails
	}
	for _, tc := range failureCases {
		fmt.Printf("\n========== SagaWithCompensationWorkflow steps=%d ==========\n", tc.steps)
		input := &SagaWithCompensationInput{
			Steps:      tc.steps,
			FailAtStep: tc.failAt,
		}
		// steps per workflow: failAt forwards + failAt compensates = 2*failAt.
		stepsPerWF := 2 * tc.failAt
		result := runBenchmark(c, cfg, SagaWithCompensationWorkflow, input, stepsPerWF,
			"SagaWithCompensationWorkflow",
			fmt.Sprintf("steps=%d", tc.steps))
		printResult(result)
	}
}
