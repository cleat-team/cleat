package embedded

import (
	"testing"
)

// ---------------------------------------------------------------------------
// setScope / getScope / clearScope on execution
// ---------------------------------------------------------------------------

func TestSetScopeGetScopeClearScope(t *testing.T) {
	r := New()
	e := newExecution(r, "test-workflow", "{}")

	// Initial scope should be empty.
	objType, instKey := e.getScope()
	if objType != "" || instKey != "" {
		t.Fatalf("expected empty initial scope, got (%q, %q)", objType, instKey)
	}

	// Set scope and verify.
	prev := e.setScope("Order", "ord-42")
	if prev != "" {
		t.Fatalf("expected empty previous scope, got %q", prev)
	}
	objType, instKey = e.getScope()
	if objType != "Order" || instKey != "ord-42" {
		t.Fatalf("expected (Order, ord-42), got (%q, %q)", objType, instKey)
	}

	// Stack-style save/restore: SetScope returns previous prefix.
	prev = e.setScope("Invoice", "inv-1")
	if prev != "vo:Order:ord-42:" {
		t.Fatalf("expected previous prefix 'vo:Order:ord-42:', got %q", prev)
	}
	objType, instKey = e.getScope()
	if objType != "Invoice" || instKey != "inv-1" {
		t.Fatalf("expected (Invoice, inv-1), got (%q, %q)", objType, instKey)
	}

	// Clear scope returns previous prefix.
	cleared := e.clearScope()
	if cleared != "vo:Invoice:inv-1:" {
		t.Fatalf("expected cleared prefix 'vo:Invoice:inv-1:', got %q", cleared)
	}
	objType, instKey = e.getScope()
	if objType != "" || instKey != "" {
		t.Fatalf("expected empty scope after clear, got (%q, %q)", objType, instKey)
	}
}

func TestSetScopeEmptyStringsClears(t *testing.T) {
	r := New()
	e := newExecution(r, "test", "{}")

	// Set scope first.
	e.setScope("Widget", "w-99")
	objType, instKey := e.getScope()
	if objType != "Widget" || instKey != "w-99" {
		t.Fatalf("expected (Widget, w-99), got (%q, %q)", objType, instKey)
	}

	// SetScope with empty strings should clear.
	prev := e.setScope("", "")
	if prev != "vo:Widget:w-99:" {
		t.Fatalf("expected previous prefix 'vo:Widget:w-99:', got %q", prev)
	}
	objType, instKey = e.getScope()
	if objType != "" || instKey != "" {
		t.Fatalf("expected empty scope after setScope(\"\", \"\"), got (%q, %q)", objType, instKey)
	}
}

// ---------------------------------------------------------------------------
// uuid on execution
// ---------------------------------------------------------------------------

func TestExecutionUUIDReturnsNonEmpty(t *testing.T) {
	r := New()
	e := newExecution(r, "test-workflow", "{}")

	u := e.uuid("seed-a")
	if u == "" {
		t.Fatal("expected non-empty UUID")
	}
}

func TestExecutionUUIDDeterministic(t *testing.T) {
	r := New()
	e := newExecution(r, "test-workflow", "{}")

	// Same seed produces same UUID.
	u1 := e.uuid("seed-a")
	u2 := e.uuid("seed-a")
	if u1 != u2 {
		t.Fatalf("expected same seed to produce same UUID, got %q vs %q", u1, u2)
	}

	// Different seed produces different UUID.
	u3 := e.uuid("seed-b")
	if u1 == u3 {
		t.Fatal("expected different seeds to produce different UUIDs")
	}
}

func TestExecutionUUIDDifferentWorkflows(t *testing.T) {
	r := New()
	e1 := newExecution(r, "wf-1", "{}")
	e2 := newExecution(r, "wf-2", "{}")

	// Same seed, different workflows should produce different UUIDs.
	u1 := e1.uuid("same-seed")
	u2 := e2.uuid("same-seed")
	if u1 == u2 {
		t.Fatal("expected different workflows to produce different UUIDs for same seed")
	}
}

// ---------------------------------------------------------------------------
// durableLog on execution (no-op, should not panic)
// ---------------------------------------------------------------------------

func TestExecutionDurableLogDoesNotPanic(t *testing.T) {
	r := New()
	e := newExecution(r, "test", "{}")

	// durableLog is a best-effort no-op; verify it does not panic.
	e.durableLog("test message")
	e.durableLog("")
	e.durableLog("multi\nline\nmessage")
}

// ---------------------------------------------------------------------------
// Runner.Signal (no-op, should not panic)
// ---------------------------------------------------------------------------

func TestRunnerSignalDoesNotPanic(t *testing.T) {
	r := New()

	// Signal is a no-op that acquires a lock; verify it does not panic.
	r.Signal("workflow-1", "signal-name", "payload")
	r.Signal("", "", "")
	r.Signal("wf-2", "sig-2", `{"key":"val"}`)
}
