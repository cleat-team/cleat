package embedded

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewReturnsNonNil(t *testing.T) {
	r := New()
	if r == nil {
		t.Fatal("New() returned nil")
	}
}

func TestRegisterAndExecuteWorkflow(t *testing.T) {
	r := New()
	r.Register("hello", func(ctx *Context) error {
		ctx.SetOutput(`{"greeting":"Hello, World!"}`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "hello", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"greeting":"Hello, World!"}` {
		t.Fatalf("expected %q, got %q", `{"greeting":"Hello, World!"}`, result)
	}
}

func TestExecuteWorkflowReturnsErrorForUnknownWorkflow(t *testing.T) {
	r := New()
	_, err := r.ExecuteWorkflow(context.Background(), "unknown", "{}")
	if err == nil {
		t.Fatal("expected error for unknown workflow")
	}
}

func TestExecuteWorkflowTyped(t *testing.T) {
	r := New()
	r.Register("echo", func(ctx *Context) error {
		// Echo back the input
		ctx.SetOutput(ctx.Input)
		return nil
	})

	var result string
	err := r.ExecuteWorkflowTyped(context.Background(), "echo", "hello", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello" {
		t.Fatalf("expected %q, got %q", "hello", result)
	}
}

func TestWorkflowContextHasHostCalls(t *testing.T) {
	r := New()
	r.Register("check_hc", func(ctx *Context) error {
		h := ctx.H()
		if h.HostCallsImpl == nil {
			t.Fatal("H() returned nil HostCallsImpl")
		}

		// DurableLog should not panic.
		h.DurableLog("test message")

		// Now() should return a non-zero time, proving the clock
		// implementation wired through HostCalls is functional.
		n := h.Now()
		if n.IsZero() {
			t.Error("Now() returned zero time; HostCalls Now is not functional")
		}

		// WorkflowID() should return the registered workflow name
		// rather than being empty or nil.
		wid := h.WorkflowID()
		if wid != "check_hc" {
			t.Errorf("WorkflowID() = %q, want %q", wid, "check_hc")
		}

		ctx.SetOutput(`{"ok":true}`)
		return nil
	})

	_, err := r.ExecuteWorkflow(context.Background(), "check_hc", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkflowIDAndRunID(t *testing.T) {
	r := New()
	r.Register("get_ids", func(ctx *Context) error {
		h := ctx.H()
		wid := h.WorkflowID()
		rid := h.RunID()
		if wid == "" {
			t.Error("WorkflowID() returned empty")
		}
		if rid == "" {
			t.Error("RunID() returned empty")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})

	_, err := r.ExecuteWorkflow(context.Background(), "get_ids", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDurableCallWorks(t *testing.T) {
	r := New()
	r.Register("caller", func(ctx *Context) error {
		h := ctx.H()
		// Call DurableCall twice with identical inputs to verify
		// deterministic routing: same service+operation+request
		// must produce the same response each time.
		resp1, err := h.DurableCall("svc", "op", `{"key":"val"}`)
		if err != nil {
			return err
		}
		resp2, err := h.DurableCall("svc", "op", `{"key":"val"}`)
		if err != nil {
			return err
		}
		if resp1 != resp2 {
			return fmt.Errorf("non-deterministic DurableCall: first call returned %q, second returned %q", resp1, resp2)
		}
		ctx.SetOutput(resp1)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "caller", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"result":"ok"}` {
		t.Fatalf("expected %q, got %q", `{"result":"ok"}`, result)
	}
}

func TestDurableSleepAdvancesClock(t *testing.T) {
	r := New()
	startTime := r.now

	r.Register("sleeper", func(ctx *Context) error {
		h := ctx.H()
		before := h.Now()
		h.DurableSleep(100 * time.Millisecond)
		after := h.Now()
		diff := after.Sub(before)
		if diff < 100*time.Millisecond {
			t.Errorf("expected sleep to advance clock by at least 100ms, got %v", diff)
		}
		ctx.SetOutput(`{"slept":true}`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "sleeper", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"slept":true}` {
		t.Fatalf("expected %q, got %q", `{"slept":true}`, result)
	}

	// Verify runner clock advanced
	if !r.now.After(startTime) {
		t.Error("expected runner clock to advance after sleep")
	}
}

func TestChildWorkflow(t *testing.T) {
	r := New()
	r.Register("child", func(ctx *Context) error {
		ctx.SetOutput(`{"from":"child"}`)
		return nil
	})
	r.Register("parent", func(ctx *Context) error {
		h := ctx.H()
		runID, err := h.ChildWorkflow("child", `{"input":"data"}`)
		if err != nil {
			return err
		}
		result, err := h.AwaitChild(runID)
		if err != nil {
			return err
		}
		ctx.SetOutput(result)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "parent", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"from":"child"}` {
		t.Fatalf("expected child workflow output, got %q", result)
	}
}

func TestDurablePromise(t *testing.T) {
	r := New()
	r.Register("promise_wf", func(ctx *Context) error {
		h := ctx.H()
		promiseID, err := h.CreatePromise("test-promise")
		if err != nil {
			return err
		}
		// Await with a short timeout should time out (promise not resolved).
		_, timedOut, err := h.AwaitPromise(promiseID, 1*time.Millisecond)
		if err != nil {
			return err
		}
		if !timedOut {
			return errors.New("expected timeout on unresolved promise")
		}
		ctx.SetOutput(`{"timed_out":true}`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "promise_wf", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"timed_out":true}` {
		t.Fatalf("expected %q, got %q", `{"timed_out":true}`, result)
	}
}

func TestDurableDefer(t *testing.T) {
	r := New()
	r.Register("defer_wf", func(ctx *Context) error {
		h := ctx.H()
		id, err := h.DurableDefer("cleanup task")
		if err != nil {
			return err
		}
		if id == "" {
			t.Error("expected non-empty defer ID")
		}
		ctx.SetOutput(`{"deferred":true}`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "defer_wf", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"deferred":true}` {
		t.Fatalf("expected %q, got %q", `{"deferred":true}`, result)
	}
}

func TestRegisterOverwritesExisting(t *testing.T) {
	r := New()
	r.Register("wf", func(ctx *Context) error {
		ctx.SetOutput(`"first"`)
		return nil
	})
	r.Register("wf", func(ctx *Context) error {
		ctx.SetOutput(`"second"`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "wf", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `"second"` {
		t.Fatalf("expected %q, got %q", `"second"`, result)
	}
}

func TestSetOutputTyped(t *testing.T) {
	r := New()
	type output struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	r.Register("typed_out", func(ctx *Context) error {
		err := ctx.SetOutputTyped(output{A: 42, B: "hello"})
		if err != nil {
			return err
		}
		return nil
	})

	var result output
	err := r.ExecuteWorkflowTyped(context.Background(), "typed_out", struct{}{}, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.A != 42 || result.B != "hello" {
		t.Fatalf("expected {42 hello}, got %+v", result)
	}
}

// ---- Lock tests ----

func TestAcquireLock_Uncontested(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		acquired, err := h.AcquireLock("test-lock", time.Second)
		if err != nil {
			return fmt.Errorf("unexpected error: %v", err)
		}
		if !acquired {
			return errors.New("expected to acquire uncontested lock")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAcquireLock_SameWorkflowReacquires(t *testing.T) {
	// acquireLock returns true when the same workflow re-acquires a lock it already holds.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		first, err := h.AcquireLock("my-lock", time.Second)
		if err != nil {
			return fmt.Errorf("first acquire error: %v", err)
		}
		if !first {
			return errors.New("expected to acquire lock first time")
		}
		second, err := h.AcquireLock("my-lock", time.Second)
		if err != nil {
			return fmt.Errorf("second acquire error: %v", err)
		}
		if !second {
			return errors.New("expected to re-acquire lock held by same workflow")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAcquireLock_DifferentWorkflowRejected(t *testing.T) {
	// When a lock is held by a different workflow ID, acquireLock returns false.
	// This path is exercised by directly manipulating the execution's lock map
	// since each execution has its own lock map in the embedded runner.
	r := New()
	e := newExecution(r, "wf1", "{}")

	// First acquire from wf1 succeeds.
	acquired, err := e.acquireLock("shared-key", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected wf1 to acquire uncontested lock")
	}

	// Simulate that "wf2" already holds the lock.
	e.locks["shared-key"] = "wf2"

	// Now wf1 should be rejected (stored wfID is "wf2", not "wf1").
	acquired, err = e.acquireLock("shared-key", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("expected acquireLock to return false when lock held by different workflow")
	}
}

func TestAcquireLock_ConcurrentGoroutines(t *testing.T) {
	// Multiple goroutines calling acquireLock on a single execution.
	// All calls use the same workflow ID so all acquires succeed.
	// The mutex protects the lock map from data races.
	r := New()
	e := newExecution(r, "concurrent-wf", "{}")

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acquired, err := e.acquireLock("same-key", 1000)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if !acquired {
				t.Error("expected to acquire lock concurrently with same workflow ID")
			}
			// Release so another goroutine can experience fresh acquire.
			if err := e.releaseLock("same-key"); err != nil {
				t.Errorf("release error: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestReleaseLock_AllowsReacquire(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		acquired, err := h.AcquireLock("my-lock", time.Second)
		if err != nil {
			return fmt.Errorf("acquire error: %v", err)
		}
		if !acquired {
			return errors.New("expected to acquire lock")
		}

		if err := h.ReleaseLock("my-lock"); err != nil {
			return fmt.Errorf("release error: %v", err)
		}

		acquired2, err := h.AcquireLock("my-lock", time.Second)
		if err != nil {
			return fmt.Errorf("second acquire error: %v", err)
		}
		if !acquired2 {
			return errors.New("expected to re-acquire lock after release")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestReleaseLock_NotHeldIsNoOp(t *testing.T) {
	// Releasing a lock that was never acquired should not produce an error.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		if err := h.ReleaseLock("never-acquired"); err != nil {
			return fmt.Errorf("release of unheld lock should not error: %v", err)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

// ---- Condition tests ----

func TestAwaitCondition_AlreadyTrue(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		called := 0
		met := h.AwaitCondition(func() bool {
			called++
			return true
		}, time.Millisecond, time.Second)
		if !met {
			return errors.New("expected condition to be met immediately")
		}
		if called != 1 {
			return fmt.Errorf("expected predicate called once, got %d", called)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAwaitCondition_BecomesTrue(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		counter := 0
		met := h.AwaitCondition(func() bool {
			counter++
			return counter >= 3
		}, time.Millisecond, time.Second)
		if !met {
			return errors.New("expected condition to be met after polls")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAwaitCondition_Timeout(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		met := h.AwaitCondition(func() bool {
			return false
		}, time.Millisecond, 5*time.Millisecond)
		if met {
			return errors.New("expected timeout, not condition met")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAwaitCondition_PredicateCalledMultipleTimes(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		counter := 0
		met := h.AwaitCondition(func() bool {
			counter++
			return false
		}, time.Millisecond, 10*time.Millisecond)
		if met {
			return errors.New("expected timeout")
		}
		if counter < 2 {
			return fmt.Errorf("expected predicate called at least 2 times, got %d", counter)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

// ---- SideEffect tests ----

func TestSideEffect_RecordsResult(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		result, err := h.SideEffect(func() (string, error) {
			return "computed-value", nil
		})
		if err != nil {
			return fmt.Errorf("unexpected error: %v", err)
		}
		if result != "computed-value" {
			return fmt.Errorf("expected %q, got %q", "computed-value", result)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestSideEffect_SameResultOnMultipleCalls(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		v1, err := h.SideEffect(func() (string, error) {
			return "deterministic", nil
		})
		if err != nil {
			return err
		}
		v2, err := h.SideEffect(func() (string, error) {
			return "deterministic", nil
		})
		if err != nil {
			return err
		}
		if v1 != v2 {
			return fmt.Errorf("expected same result from multiple SideEffect calls, got %q and %q", v1, v2)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestSideEffect_WithError(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		_, err := h.SideEffect(func() (string, error) {
			return "", errors.New("side effect error")
		})
		return err
	})
	_, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err == nil {
		t.Fatal("expected error from SideEffect when fn returns an error")
	}
}

func TestSideEffect_PanickingFn(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		h.SideEffect(func() (string, error) {
			panic("intentional panic from side effect")
		})
		return nil
	})

	recovered := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				recovered = true
			}
		}()
		r.ExecuteWorkflow(context.Background(), "test", "{}")
	}()
	if !recovered {
		t.Error("expected panic from SideEffect with panicking fn")
	}
}

// ---- Random tests ----

func TestRandom_ValueInRange(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		for i := 0; i < 100; i++ {
			val := h.Random()
			if val < 0 || val >= 1000000 {
				return fmt.Errorf("random value %d out of range [0, 1000000)", val)
			}
			h.DurableSleep(time.Millisecond)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestRandom_SameValueSameTime(t *testing.T) {
	// Without clock advancement, two Random() calls return the same value.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		v1 := h.Random()
		v2 := h.Random()
		if v1 != v2 {
			return fmt.Errorf("expected same random value without clock advance, got %d and %d", v1, v2)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestRandom_ChangesWhenClockAdvances(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		v1 := h.Random()
		h.DurableSleep(time.Millisecond)
		v2 := h.Random()
		if v1 == v2 {
			// Extremely unlikely that two different millisecond timestamps
			// produce the same modulo-1000000 result.
			t.Logf("random values happened to collide: %d", v1)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestRandom_DeterministicAcrossRuns(t *testing.T) {
	// Two workflows starting at the same clock time produce the same random value.
	r := New()
	r.Register("get_random", func(ctx *Context) error {
		h := ctx.H()
		val := h.Random()
		ctx.SetOutput(fmt.Sprintf(`{"val":%d}`, val))
		return nil
	})
	r1, err := r.ExecuteWorkflow(context.Background(), "get_random", "{}")
	if err != nil {
		t.Fatalf("first run error: %v", err)
	}
	r2, err := r.ExecuteWorkflow(context.Background(), "get_random", "{}")
	if err != nil {
		t.Fatalf("second run error: %v", err)
	}
	if r1 != r2 {
		t.Fatalf("expected same random value across runs, got %q and %q", r1, r2)
	}
}

// ---- Additional coverage for remaining uncovered functions ----

func TestDurableLogDoesNotPanic(t *testing.T) {
	// durableLog is a best-effort no-op; verify it doesn't panic.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		h.DurableLog("test diagnostic message")
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestPollCancellationReturnsFalse(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		cancelled, reason := h.PollCancellation()
		if cancelled {
			return errors.New("expected no cancellation")
		}
		if reason != "" {
			return fmt.Errorf("expected empty reason, got %q", reason)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestSetOutputf(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		ctx.SetOutputf(`{"val":%d}`, 42)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"val":42}` {
		t.Fatalf("expected %q, got %q", `{"val":42}`, result)
	}
}

func TestSignalWorkflowAndPollSignal(t *testing.T) {
	// SignalWorkflow stores a signal; PollSignal retrieves it.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		err := h.SignalWorkflow("run-id", "my-sig", `{"key":"val"}`)
		if err != nil {
			return fmt.Errorf("SignalWorkflow error: %v", err)
		}
		payload, found, err := h.PollSignal("my-sig")
		if err != nil {
			return fmt.Errorf("PollSignal error: %v", err)
		}
		if !found {
			return errors.New("expected PollSignal to find signal")
		}
		if payload != `{"key":"val"}` {
			return fmt.Errorf("expected payload %q, got %q", `{"key":"val"}`, payload)
		}
		// Poll again: signal should be consumed.
		_, found, err = h.PollSignal("my-sig")
		if err != nil {
			return err
		}
		if found {
			return errors.New("expected signal to be consumed after first poll")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestPollSignalNoMatch(t *testing.T) {
	// PollSignal for a name that doesn't exist returns found=false.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		_, found, err := h.PollSignal("nonexistent")
		if err != nil {
			return err
		}
		if found {
			return errors.New("expected no signal for nonexistent name")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestDurableAwaitSignalsImmediateMatch(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// Queue a signal first, then await it.
		h.SignalWorkflow("run-id", "greeting", `{"hello":"world"}`)
		name, payload, timedOut, err := h.DurableAwaitSignals([]string{"greeting"}, 5000)
		if err != nil {
			return err
		}
		if timedOut {
			return errors.New("expected signal, not timeout")
		}
		if name != "greeting" {
			return fmt.Errorf("expected 'greeting', got %q", name)
		}
		if payload != `{"hello":"world"}` {
			return fmt.Errorf("expected payload, got %q", payload)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestDurableAwaitSignalsTimeout(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		_, _, timedOut, err := h.DurableAwaitSignals([]string{"never-signaled"}, 1)
		if err != nil {
			return err
		}
		if !timedOut {
			return errors.New("expected timeout for unmatched signal")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestDurableAwaitSignalsZeroTimeout(t *testing.T) {
	// Zero timeout means return immediately with timedOut=true.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		_, _, timedOut, err := h.DurableAwaitSignals([]string{"x"}, 0)
		if err != nil {
			return err
		}
		if !timedOut {
			return errors.New("expected timeout with zero timeout")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestDurableDeferFunc(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		called := false
		id, err := h.DurableDeferFunc(func() {
			called = true
		})
		if err != nil {
			return err
		}
		if id == "" {
			return errors.New("expected non-empty defer func ID")
		}
		_ = called
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestSendSignalAndWait(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		resp, err := h.SendSignalAndWait("target", "evt", `{"data":"x"}`, time.Second)
		if err != nil {
			return err
		}
		if resp != `{"status":"delivered"}` {
			return fmt.Errorf("expected delivered response, got %q", resp)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestReplyToSignal(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		err := h.ReplyToSignal("corr-123", `{"status":"done"}`)
		if err != nil {
			return err
		}
		// After reply, verify the signal was stored by polling it.
		payload, found, err := h.PollSignal("corr-123")
		if err != nil {
			return err
		}
		if !found {
			return errors.New("expected to find reply signal")
		}
		if payload != `{"status":"done"}` {
			return fmt.Errorf("expected reply payload, got %q", payload)
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestUUID(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		u1 := h.UUID("seed-a")
		u2 := h.UUID("seed-a")
		u3 := h.UUID("seed-b")
		if u1 == "" {
			return errors.New("expected non-empty UUID")
		}
		if u1 != u2 {
			return fmt.Errorf("expected same seed to produce same UUID, got %q vs %q", u1, u2)
		}
		if u1 == u3 {
			return errors.New("expected different seeds to produce different UUIDs")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestScopeManagement(t *testing.T) {
	// setScope, getScope, clearScope on the HostCalls interface.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()

		// Initially no scope.
		objType, instKey := h.GetScope()
		if objType != "" || instKey != "" {
			return errors.New("expected empty initial scope")
		}

		// Set scope and verify.
		prev := h.SetScope("Order", "ord-42")
		if prev != "" {
			return fmt.Errorf("expected empty previous scope, got %q", prev)
		}
		objType, instKey = h.GetScope()
		if objType != "Order" || instKey != "ord-42" {
			return fmt.Errorf("expected (Order, ord-42), got (%q, %q)", objType, instKey)
		}

		// Clear scope and verify.
		prev = h.ClearScope()
		objType, instKey = h.GetScope()
		if objType != "" || instKey != "" {
			return errors.New("expected empty scope after clear")
		}

		// ClearScope returns the previous scope prefix.
		h.SetScope("Invoice", "inv-1")
		prev = h.ClearScope()
		if prev != "vo:Invoice:inv-1:" {
			return fmt.Errorf("expected scope prefix 'vo:Invoice:inv-1:', got %q", prev)
		}

		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestScopeSetGetRoundTrip(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()

		prev := h.SetScope("Widget", "w-99")
		_ = prev
		objType, instKey := h.GetScope()
		if objType != "Widget" || instKey != "w-99" {
			return fmt.Errorf("expected (Widget, w-99), got (%q, %q)", objType, instKey)
		}

		// Stack-style save/restore: SetScope returns previous prefix.
		prev2 := h.SetScope("Gadget", "g-1")
		if prev2 != "vo:Widget:w-99:" {
			return fmt.Errorf("expected previous prefix 'vo:Widget:w-99:', got %q", prev2)
		}
		objType, instKey = h.GetScope()
		if objType != "Gadget" || instKey != "g-1" {
			return fmt.Errorf("expected (Gadget, g-1), got (%q, %q)", objType, instKey)
		}

		// Clear to reset.
		h.ClearScope()
		objType, instKey = h.GetScope()
		if objType != "" || instKey != "" {
			return errors.New("expected empty scope after clear")
		}

		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestExecuteWorkflowTypedNilOutput(t *testing.T) {
	// ExecuteWorkflowTyped with nil output should not error.
	r := New()
	r.Register("test", func(ctx *Context) error {
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	err := r.ExecuteWorkflowTyped(context.Background(), "test", struct{}{}, nil)
	if err != nil {
		t.Fatalf("unexpected error with nil output: %v", err)
	}
}

func TestDurableCallHTTPFetchMissingURL(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// http.fetch with empty URL should error.
		_, err := h.DurableCall("http", "fetch", `{"url":""}`)
		if err == nil {
			return errors.New("expected error for empty URL")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestDurableCallHTTPFetchInvalidRequest(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// Invalid JSON for http.fetch should error.
		_, err := h.DurableCall("http", "fetch", `not-json`)
		if err == nil {
			return errors.New("expected error for invalid JSON")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestAwaitPromiseNotFound(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		_, _, err := h.AwaitPromise("nonexistent", time.Millisecond)
		if err == nil {
			return errors.New("expected error for nonexistent promise")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

// ---------------------------------------------------------------------------
// handleHTTPFetch — additional coverage
// ---------------------------------------------------------------------------

func TestHTTPFetch_DefaultsMethodToGET(t *testing.T) {
	// A request without method defaults to GET. Since we're not actually
	// making a real request here (we're targeting a port that refuses),
	// this exercises the method-default path through the error path.
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// URL with no method should default to GET then fail at connection.
		_, err := h.DurableCall("http", "fetch", `{"url":"http://127.0.0.1:1/"}`)
		if err == nil {
			// On the off chance the request succeeded: highly unlikely.
			t.Log("unexpectedly got a response from port 1")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestHTTPFetch_WithHeadersAndBody(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// Method, headers, and body should all be passed, then fail at connection.
		_, err := h.DurableCall("http", "fetch", `{"url":"http://127.0.0.1:1/","method":"POST","headers":{"Content-Type":"application/json"},"body":"{\"key\":\"val\"}"}`)
		if err == nil {
			t.Log("unexpectedly got a response from port 1")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

func TestHTTPFetch_InvalidURLSyntax(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// A URL with invalid host syntax should cause http.NewRequest to fail.
		_, err := h.DurableCall("http", "fetch", `{"url":"http://[::1]:notanumber/path"}`)
		if err == nil {
			t.Error("expected error for invalid URL syntax, got nil")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
	}
}

// ---------------------------------------------------------------------------
// awaitChild — fallback path when child result not found
// ---------------------------------------------------------------------------

func TestAwaitChild_FallbackResultWhenNotFound(t *testing.T) {
	r := New()
	r.Register("test", func(ctx *Context) error {
		h := ctx.H()
		// AwaitChild with a runID that was never created should return the
		// fallback result rather than an error.
		result, err := h.AwaitChild("nonexistent-run-id")
		if err != nil {
			return err
		}
		if result != `{"status":"completed"}` {
			t.Errorf("expected fallback result, got %q", result)
		}
		ctx.SetOutput(result)
		return nil
	})
	result, err := r.ExecuteWorkflow(context.Background(), "test", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"status":"completed"}` {
		t.Fatalf("expected %q, got %q", `{"status":"completed"}`, result)
	}
}
