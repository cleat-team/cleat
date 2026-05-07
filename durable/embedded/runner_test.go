package embedded

import (
	"context"
	"errors"
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
		if h == nil {
			t.Fatal("H() returned nil")
		}
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})

	result, err := r.ExecuteWorkflow(context.Background(), "check_hc", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("expected %q, got %q", `{"ok":true}`, result)
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
		resp, err := h.DurableCall("svc", "op", `{"key":"val"}`)
		if err != nil {
			return err
		}
		ctx.SetOutput(resp)
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
