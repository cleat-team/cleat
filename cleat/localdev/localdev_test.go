package localdev

import (
	"io"
	"testing"
)

func TestNewLocalRunner_CreatesNonNil(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
}

func TestNewLocalRunner_HEqualsNonNil(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	h := r.H()
	if h == nil {
		t.Fatal("H() returned nil")
	}
}

func TestNewLocalRunner_EventsStartEmpty(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	events := r.Events()
	if len(events) != 0 {
		t.Errorf("expected empty events, got %d", len(events))
	}
}

func TestNewLocalRunner_ContinueAsNewInputInitiallyEmpty(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	input, ok := r.ContinueAsNewInput()
	if ok {
		t.Errorf("expected ok=false initially, got ok=%v with input=%q", ok, input)
	}
	if input != "" {
		t.Errorf("expected empty input, got %q", input)
	}
}

func TestNewLocalRunner_WithWorkflowID(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("test-wf-123"),
	)
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
	// WorkflowID isn't exported on LocalRunner, but we can verify
	// that the runner is functional.
	h := r.H()
	if h == nil {
		t.Fatal("H() returned nil")
	}
}

func TestNewLocalRunner_AcquireConcurrencyKey(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("wf-1"),
	)
	acquired, err := r.AcquireConcurrencyKey("key-1", "wf-1", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire concurrency key")
	}

	// Second acquisition by same workflow should succeed.
	reAcquired, err := r.AcquireConcurrencyKey("key-1", "wf-1", 0)
	if err != nil {
		t.Fatalf("re-acquire returned error: %v", err)
	}
	if !reAcquired {
		t.Fatal("expected re-acquire by same workflow to succeed")
	}

	// Different workflow should fail.
	blocked, err := r.AcquireConcurrencyKey("key-1", "wf-2", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey by different workflow returned error: %v", err)
	}
	if blocked {
		t.Fatal("expected different workflow to be blocked")
	}
}

func TestNewLocalRunner_ReleaseConcurrencyKeys(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("wf-release"),
	)

	r.AcquireConcurrencyKey("key-a", "wf-release", 0)
	r.AcquireConcurrencyKey("key-b", "wf-release", 0)

	r.ReleaseConcurrencyKeys("wf-release")

	// Another workflow should now be able to acquire.
	acquired, err := r.AcquireConcurrencyKey("key-a", "wf-other", 0)
	if err != nil {
		t.Fatalf("acquire after release returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire key after release")
	}
}
