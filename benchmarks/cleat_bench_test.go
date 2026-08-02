// Package benchmarks provides performance benchmarks for the cleat durable
// workflow engine. These benchmarks use an in-process HostCalls implementation
// (via cleat.NewHostCalls) that avoids WASM compilation overhead, measuring
// pure framework throughput.
//
// Usage:
//
//	go test -bench=. -benchmem -benchtime=10s ./benchmarks/
//
// Each benchmark reports:
//   - wf/s     workflows per second
//   - steps/s  durable steps (API calls) per second
//   - ns/op    nanoseconds per workflow (standard testing.B metric)
//
// To compare with Temporal or DBOS, port the workflow functions in
// benchmarks/workflows/ to the respective SDK and run equivalent benchmarks.
package benchmarks

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/benchmarks/workflows"
	"github.com/cleat-team/cleat/cleat"
)

// ---------------------------------------------------------------------------
// Benchmark harness
// ---------------------------------------------------------------------------

// benchHarness provides a HostCalls implementation for in-process
// benchmarking. It tracks call counts and manages child workflow state.
// All operations are in-memory with no external dependencies.
type benchHarness struct {
	mu    sync.Mutex
	wfID  string
	runID string
	nowMs int64
	calls int64

	// Cached HostCalls instance (function closures capture bh pointer).
	h cleat.HostCalls

	// Registered child workflow functions.
	childFuncs map[string]func(cleat.HostCalls, string) (string, error)

	// Child workflow result storage (keyed by runID).
	childResults map[string]string
	childErrs    map[string]error
}

// newBenchHarness creates a new benchmark harness with the given child
// workflow registrations. Pass nil if no child workflows are needed.
func newBenchHarness(childFuncs map[string]func(cleat.HostCalls, string) (string, error)) *benchHarness {
	bh := &benchHarness{
		wfID:         "bench-wf",
		runID:        "bench-run-00000000-0000-0000-0000-000000000000",
		nowMs:        1704067200000, // 2024-01-01T00:00:00Z
		childFuncs:   childFuncs,
		childResults: make(map[string]string),
		childErrs:    make(map[string]error),
	}
	// Build the HostCalls once and cache it. Method-value closures capture
	// the bh pointer, so calls go to the current bh state even after reset().
	bh.h = cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall:      bh.durableCall,
		DurableSleep:     bh.durableSleep,
		WorkflowID:       bh.workflowID,
		RunID:            bh.runIDFn,
		Now:              bh.nowMsFn,
		Random:           bh.randomFn,
		ChildWorkflow:    bh.childWorkflow,
		AwaitChild:       bh.awaitChild,
		AwaitAllChildren: bh.awaitAllChildren,
		PluginCall:       bh.pluginCall,
		Version:          bh.version,
		MinVersion:       bh.minVersion,
		SetQueryState:    bh.setQueryState,
		DurableLog:       bh.durableLog,
		PollCancellation: bh.pollCancellation,
		PollSignal:       bh.pollSignal,
		DurableDefer:     bh.durableDefer,
	})
	return bh
}

// reset clears all per-iteration mutable state.
func (bh *benchHarness) reset() {
	bh.mu.Lock()
	defer bh.mu.Unlock()
	bh.calls = 0
	bh.childResults = make(map[string]string)
	bh.childErrs = make(map[string]error)
}

// H returns the HostCalls interface for this harness.
func (bh *benchHarness) H() cleat.HostCalls {
	return bh.h
}

// callCount returns the total number of DurableCall invocations since the
// last reset.
func (bh *benchHarness) callCount() int64 {
	bh.mu.Lock()
	defer bh.mu.Unlock()
	return bh.calls
}

// ---------------------------------------------------------------------------
// HostCalls implementations
// ---------------------------------------------------------------------------

func (bh *benchHarness) durableCall(service, operation, requestJSON string) (string, error) {
	bh.mu.Lock()
	bh.calls++
	bh.mu.Unlock()
	return `{"status":"ok"}`, nil
}

func (bh *benchHarness) durableSleep(ms int64) {
	// Advance simulated clock rather than blocking.
	bh.mu.Lock()
	bh.nowMs += ms
	bh.mu.Unlock()
}

func (bh *benchHarness) workflowID() string {
	return bh.wfID
}

func (bh *benchHarness) runIDFn() string {
	return bh.runID
}

func (bh *benchHarness) nowMsFn() int64 {
	bh.mu.Lock()
	defer bh.mu.Unlock()
	return bh.nowMs
}

func (bh *benchHarness) randomFn() int64 {
	return 42 // deterministic
}

func (bh *benchHarness) version() int              { return 1 }
func (bh *benchHarness) minVersion() int           { return 1 }
func (bh *benchHarness) setQueryState(_, _ string) {}

func (bh *benchHarness) durableLog(_ string) {}

func (bh *benchHarness) pollCancellation() (bool, string) {
	return false, ""
}

func (bh *benchHarness) pollSignal(_ string) (string, bool, error) {
	return "", false, nil
}

func (bh *benchHarness) durableDefer(_ string) (string, error) {
	return "defer-0", nil
}

func (bh *benchHarness) childWorkflow(name, inputJSON string) (string, error) {
	fn, ok := bh.childFuncs[name]
	if !ok {
		return "", fmt.Errorf("bench: unknown child workflow %q", name)
	}

	bh.mu.Lock()
	runID := fmt.Sprintf("child-%s-%d", name, len(bh.childResults))
	bh.mu.Unlock()

	// Execute the child inline (synchronous). In the embedded/workflow
	// model children run sequentially within the parent's event loop, so
	// this is faithful to real execution.
	result, err := fn(bh.H(), inputJSON)

	bh.mu.Lock()
	bh.childResults[runID] = result
	if err != nil {
		bh.childErrs[runID] = err
	}
	bh.mu.Unlock()

	return runID, nil
}

func (bh *benchHarness) awaitChild(runID string) (string, error) {
	bh.mu.Lock()
	result, ok := bh.childResults[runID]
	err := bh.childErrs[runID]
	bh.mu.Unlock()

	if !ok {
		// Child may not have been started in this iteration; treat as
		// already completed (safest default for benchmarking).
		return `{"status":"completed"}`, nil
	}
	if err != nil {
		return "", err
	}
	return result, nil
}

func (bh *benchHarness) awaitAllChildren(runIDs []string) ([]cleat.ChildResult, error) {
	results := make([]cleat.ChildResult, len(runIDs))
	for i, runID := range runIDs {
		result, err := bh.awaitChild(runID)
		if err != nil {
			results[i] = cleat.ChildResult{RunID: runID, Error: err.Error()}
		} else {
			results[i] = cleat.ChildResult{RunID: runID, Result: result}
		}
	}
	return results, nil
}

func (bh *benchHarness) pluginCall(pluginName, functionName, inputJSON string) (string, error) {
	return fmt.Sprintf(`{"status":"ok","plugin":%q,"func":%q}`, pluginName, functionName), nil
}

// ---------------------------------------------------------------------------
// Benchmark helpers
// ---------------------------------------------------------------------------

// reportMetrics records workflows-per-second and steps-per-second for a
// completed benchmark loop. Call at the end of each benchmark function,
// after b.ResetTimer() and the b.N loop.
func reportMetrics(b *testing.B, start time.Time, totalSteps float64) {
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return
	}
	iter := float64(b.N)
	b.ReportMetric(iter/elapsed.Seconds(), "wf/s")
	b.ReportMetric(totalSteps/elapsed.Seconds(), "steps/s")
}

// ---------------------------------------------------------------------------
// Simple (sequential) workflow benchmarks
// ---------------------------------------------------------------------------

// BenchmarkSimpleWorkflow measures steps/sec for a simple linear workflow
// with N sequential DurableCall steps.
func BenchmarkSimpleWorkflow(b *testing.B) {
	b.Run("steps=10", func(b *testing.B) {
		benchmarkSimple(b, 10)
	})
	b.Run("steps=100", func(b *testing.B) {
		benchmarkSimple(b, 100)
	})
	b.Run("steps=1000", func(b *testing.B) {
		benchmarkSimple(b, 1000)
	})
}

func benchmarkSimple(b *testing.B, steps int) {
	bh := newBenchHarness(nil)
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		bh.reset()
		_, err := workflows.SimpleWorkflow(bh.H(), workflows.SimpleInput{Steps: steps})
		if err != nil {
			b.Fatalf("SimpleWorkflow failed: %v", err)
		}
	}

	reportMetrics(b, start, float64(steps*b.N))
}

// ---------------------------------------------------------------------------
// Fan-out workflow benchmarks
// ---------------------------------------------------------------------------

// BenchmarkFanOutWorkflow measures fan-out to N parallel child workflows.
func BenchmarkFanOutWorkflow(b *testing.B) {
	b.Run("children=10", func(b *testing.B) {
		benchmarkFanOut(b, 10)
	})
	b.Run("children=100", func(b *testing.B) {
		benchmarkFanOut(b, 100)
	})
	b.Run("children=500", func(b *testing.B) {
		benchmarkFanOut(b, 500)
	})
}

func benchmarkFanOut(b *testing.B, children int) {
	childFuncs := map[string]func(cleat.HostCalls, string) (string, error){
		"noop_child": workflows.NoopChild,
	}
	bh := newBenchHarness(childFuncs)
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		bh.reset()
		_, err := workflows.FanOutWorkflow(bh.H(), workflows.FanOutInput{Children: children})
		if err != nil {
			b.Fatalf("FanOutWorkflow failed: %v", err)
		}
	}

	// Each workflow does (children) ChildWorkflow calls + (children) child
	// DurableCalls + 1 AwaitAllChildren call = 2*children + 1 steps.
	reportMetrics(b, start, float64((2*children+1)*b.N))
}

// ---------------------------------------------------------------------------
// Saga (compensation) workflow benchmarks
// ---------------------------------------------------------------------------

// BenchmarkSagaWorkflow measures saga with N compensation steps (happy path).
func BenchmarkSagaWorkflow(b *testing.B) {
	b.Run("steps=10", func(b *testing.B) {
		benchmarkSaga(b, 10)
	})
	b.Run("steps=100", func(b *testing.B) {
		benchmarkSaga(b, 100)
	})
	b.Run("steps=1000", func(b *testing.B) {
		benchmarkSaga(b, 1000)
	})
}

func benchmarkSaga(b *testing.B, steps int) {
	bh := newBenchHarness(nil)
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		bh.reset()
		_, err := workflows.SagaWorkflow(bh.H(), workflows.SagaInput{Steps: steps})
		if err != nil {
			b.Fatalf("SagaWorkflow failed: %v", err)
		}
	}

	// Each saga step calls DurableCall once in the forward function.
	reportMetrics(b, start, float64(steps*b.N))
}

// BenchmarkSagaWithCompensation measures saga where one step fails and all
// previous steps are compensated.
func BenchmarkSagaWithCompensation(b *testing.B) {
	b.Run("steps=10", func(b *testing.B) {
		benchmarkSagaCompensation(b, 10)
	})
	b.Run("steps=100", func(b *testing.B) {
		benchmarkSagaCompensation(b, 100)
	})
}

func benchmarkSagaCompensation(b *testing.B, steps int) {
	bh := newBenchHarness(nil)
	b.ResetTimer()
	start := time.Now()

	failAt := steps - 1 // make the last step fail
	for i := 0; i < b.N; i++ {
		bh.reset()
		_, err := workflows.SagaWithCompensationWorkflow(bh.H(), workflows.SagaWithCompensationInput{
			Steps:      steps,
			FailAtStep: failAt,
		})
		if err != nil {
			// SagaWithCompensationWorkflow returns a valid output (no error)
			// even when compensation happens. The error from Run is handled
			// internally. If we get here, something else went wrong.
			b.Fatalf("SagaWithCompensationWorkflow failed: %v", err)
		}
	}

	// Steps: (failAt) forwards + (failAt) compensates = 2*failAt durable calls.
	compensatedSteps := 2 * failAt
	reportMetrics(b, start, float64(compensatedSteps*b.N))
}

// ---------------------------------------------------------------------------
// AI agent loop workflow benchmarks
// ---------------------------------------------------------------------------

// BenchmarkLLMCallWorkflow measures workflow with simulated LLM calls and
// tool invocations.
func BenchmarkLLMCallWorkflow(b *testing.B) {
	b.Run("prompts=1_tools=5", func(b *testing.B) {
		benchmarkLLM(b, 1, 5)
	})
	b.Run("prompts=5_tools=3", func(b *testing.B) {
		benchmarkLLM(b, 5, 3)
	})
	b.Run("prompts=10_tools=2", func(b *testing.B) {
		benchmarkLLM(b, 10, 2)
	})
	b.Run("prompts=50_tools=1", func(b *testing.B) {
		benchmarkLLM(b, 50, 1)
	})
}

func benchmarkLLM(b *testing.B, prompts, toolsPerPrompt int) {
	bh := newBenchHarness(nil)
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		bh.reset()
		_, err := workflows.LLMWorkflow(bh.H(), workflows.LLMInput{
			Prompts:        prompts,
			ToolsPerPrompt: toolsPerPrompt,
		})
		if err != nil {
			b.Fatalf("LLMWorkflow failed: %v", err)
		}
	}

	callsPerWF := prompts * (1 + toolsPerPrompt)
	reportMetrics(b, start, float64(callsPerWF*b.N))
}
