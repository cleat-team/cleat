// Temporal Go SDK implementation of LLMWorkflow.
//
// Mirrors the Cleat LLMWorkflow in benchmarks/workflows/llm.go:
// N prompts, each with M tool invocations.  Simulates an AI agent
// loop: for each "prompt" iteration, one activity models an LLM
// chat call, followed by M activities modelling tool invocations.
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
// Types — mirrors benchmarks/workflows/llm.go
// ---------------------------------------------------------------------------

// LLMInput configures the AI agent loop simulation.
type LLMInput struct {
	Prompts        int `json:"prompts"`
	ToolsPerPrompt int `json:"tools_per_prompt"`
}

// LLMOutput is the result of the LLM simulation workflow.
type LLMOutput struct {
	TotalCalls int `json:"total_calls"`
}

// ---------------------------------------------------------------------------
// Activities
// ---------------------------------------------------------------------------

// LLMChatActivity simulates an LLM chat call (e.g., to GPT-4, Claude).
// In a real workflow this would make an HTTP call; in the benchmark it
// returns immediately to measure framework overhead.
func LLMChatActivity(ctx context.Context, prompt string) (string, error) {
	return fmt.Sprintf(`{"response":"simulated_response_for_%s"}`, prompt), nil
}

// ToolInvokeActivity simulates a tool invocation triggered by an LLM response.
func ToolInvokeActivity(ctx context.Context, toolName string, iteration int) (string, error) {
	return fmt.Sprintf(`{"result":"ok","tool":%q,"iteration":%d}`, toolName, iteration), nil
}

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

// LLMWorkflow simulates an AI agent loop with LLM calls and tool invocations.
// Each "prompt" involves one LLM chat activity followed by multiple tool
// invocation activities.  This mirrors the pattern in the Cleat LLMWorkflow:
//
//	for i := 0; i < Prompts; i++ {
//	    h.DurableCall("llm", "chat", ...)       // the LLM call
//	    for j := 0; j < ToolsPerPrompt; j++ {
//	        h.DurableCall("tools", "invoke", ...) // tool invocations
//	    }
//	}
func LLMWorkflow(ctx workflow.Context, input LLMInput) (LLMOutput, error) {
	totalCalls := 0

	activityOpts := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    5 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Second,
	})

	for i := 0; i < input.Prompts; i++ {
		// LLM chat call
		prompt := fmt.Sprintf("benchmark_prompt_%d", i)
		var chatResult string
		if err := workflow.ExecuteActivity(activityOpts, "llmChatActivity", prompt).Get(activityOpts, &chatResult); err != nil {
			return LLMOutput{}, fmt.Errorf("prompt %d llm chat: %w", i, err)
		}
		totalCalls++

		// Tool invocations from the LLM response
		for j := 0; j < input.ToolsPerPrompt; j++ {
			toolName := fmt.Sprintf("bench_tool_%d", j)
			var toolResult string
			if err := workflow.ExecuteActivity(activityOpts, "toolInvokeActivity", toolName, i).Get(activityOpts, &toolResult); err != nil {
				return LLMOutput{}, fmt.Errorf("prompt %d tool %d: %w", i, j, err)
			}
			totalCalls++
		}
	}

	return LLMOutput{TotalCalls: totalCalls}, nil
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

	taskQueue := "benchmark-llm"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(LLMWorkflow)
	w.RegisterActivity(LLMChatActivity)
	w.RegisterActivity(ToolInvokeActivity)

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
	type llmCase struct {
		prompts int
		tools   int
		label   string
	}
	testCases := []llmCase{
		{prompts: 1, tools: 5, label: "prompts=1_tools=5"},
		{prompts: 5, tools: 3, label: "prompts=5_tools=3"},
		{prompts: 10, tools: 2, label: "prompts=10_tools=2"},
		{prompts: 50, tools: 1, label: "prompts=50_tools=1"},
	}

	for _, tc := range testCases {
		fmt.Printf("\n========== LLMWorkflow %s ==========\n", tc.label)
		input := &LLMInput{
			Prompts:        tc.prompts,
			ToolsPerPrompt: tc.tools,
		}
		// steps per workflow: prompts * (1 chat + tools) = total activity calls.
		stepsPerWF := tc.prompts * (1 + tc.tools)
		result := runBenchmark(c, cfg, LLMWorkflow, input, stepsPerWF, "LLMWorkflow", tc.label)
		printResult(result)
	}
}
