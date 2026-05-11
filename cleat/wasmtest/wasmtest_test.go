package wasmtest

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// InMemorySignalStore tests
// ---------------------------------------------------------------------------

func TestInMemorySignalStore_DeliverAndPoll(t *testing.T) {
	s := NewInMemorySignalStore()
	ctx := context.Background()

	// Deliver a signal.
	if err := s.DeliverSignal(ctx, "wf-1", "payment-received", `{"amount":100}`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// Poll for it.
	payload, found, err := s.PollSignal(ctx, "wf-1", "payment-received")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found")
	}
	if payload != `{"amount":100}` {
		t.Fatalf("expected payload %q, got %q", `{"amount":100}`, payload)
	}

	// Second poll should not find it (consumed).
	_, found, err = s.PollSignal(ctx, "wf-1", "payment-received")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if found {
		t.Fatal("expected signal to be consumed after first poll")
	}
}

func TestInMemorySignalStore_PollNonExistent(t *testing.T) {
	s := NewInMemorySignalStore()
	ctx := context.Background()

	payload, found, err := s.PollSignal(ctx, "wf-1", "nonexistent")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if found {
		t.Fatal("expected not found")
	}
	if payload != "" {
		t.Fatalf("expected empty payload, got %q", payload)
	}
}

func TestInMemorySignalStore_Cancellation(t *testing.T) {
	s := NewInMemorySignalStore()
	ctx := context.Background()

	// Initially not cancelled.
	cancelled, reason, err := s.PollCancellation(ctx, "wf-1")
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if cancelled {
		t.Fatal("expected not cancelled initially")
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}

	// Set cancelled.
	s.SetCancelled("wf-1", "user requested")
	cancelled, reason, err = s.PollCancellation(ctx, "wf-1")
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if !cancelled {
		t.Fatal("expected cancelled")
	}
	if reason != "user requested" {
		t.Fatalf("expected reason %q, got %q", "user requested", reason)
	}

	// Clear cancelled.
	s.ClearCancelled("wf-1")
	cancelled, reason, err = s.PollCancellation(ctx, "wf-1")
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if cancelled {
		t.Fatal("expected not cancelled after clear")
	}
}

// ---------------------------------------------------------------------------
// InMemoryPromiseStore tests
// ---------------------------------------------------------------------------

func TestInMemoryPromiseStore_CreateAndGet(t *testing.T) {
	s := NewInMemoryPromiseStore()
	ctx := context.Background()

	if err := s.CreatePromise(ctx, "wf-1", "my-promise", "prom-1"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	status, result, errMsg, err := s.GetPromise(ctx, "wf-1", "prom-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Fatalf("expected status pending, got %q", status)
	}
	if result != "" {
		t.Fatalf("expected empty result, got %q", result)
	}
	if errMsg != "" {
		t.Fatalf("expected empty errMsg, got %q", errMsg)
	}
}

func TestInMemoryPromiseStore_Resolve(t *testing.T) {
	s := NewInMemoryPromiseStore()
	ctx := context.Background()

	s.CreatePromise(ctx, "wf-1", "my-promise", "prom-1")
	if err := s.ResolvePromise(ctx, "wf-1", "prom-1", `{"status":"done"}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	status, result, _, err := s.GetPromise(ctx, "wf-1", "prom-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "resolved" {
		t.Fatalf("expected resolved, got %q", status)
	}
	if result != `{"status":"done"}` {
		t.Fatalf("expected result %q, got %q", `{"status":"done"}`, result)
	}
}

func TestInMemoryPromiseStore_Reject(t *testing.T) {
	s := NewInMemoryPromiseStore()
	ctx := context.Background()

	s.CreatePromise(ctx, "wf-1", "my-promise", "prom-1")
	if err := s.RejectPromise(ctx, "wf-1", "prom-1", "something went wrong"); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	status, _, errMsg, err := s.GetPromise(ctx, "wf-1", "prom-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "rejected" {
		t.Fatalf("expected rejected, got %q", status)
	}
	if errMsg != "something went wrong" {
		t.Fatalf("expected errMsg %q, got %q", "something went wrong", errMsg)
	}
}

// ---------------------------------------------------------------------------
// InMemoryChildWorkflowStore tests
// ---------------------------------------------------------------------------

func TestInMemoryChildWorkflowStore_StartAndGetResult(t *testing.T) {
	s := NewInMemoryChildWorkflowStore()
	ctx := context.Background()

	runID, err := s.StartChildWorkflow(ctx, "parent-1", "child-workflow", `{"input":"test"}`, 0, "")
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if runID == "" {
		t.Fatal("expected non-empty runID")
	}

	result, completed, err := s.GetChildResult(ctx, runID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !completed {
		t.Fatal("expected completed")
	}
	if result != `{"status":"completed"}` {
		t.Fatalf("expected default result, got %q", result)
	}

	// Verify invocation was recorded.
	inv := s.Invocations()
	if len(inv) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(inv))
	}
	if inv[0].Name != "child-workflow" {
		t.Fatalf("expected name child-workflow, got %q", inv[0].Name)
	}
	if inv[0].InputJSON != `{"input":"test"}` {
		t.Fatalf("expected input %q, got %q", `{"input":"test"}`, inv[0].InputJSON)
	}
}

func TestInMemoryChildWorkflowStore_PreconfiguredResult(t *testing.T) {
	s := NewInMemoryChildWorkflowStore()
	ctx := context.Background()

	s.SetResult("child-workflow", `{"custom":"result"}`)
	runID, err := s.StartChildWorkflow(ctx, "parent-1", "child-workflow", `{}`, 0, "")
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	result, completed, err := s.GetChildResult(ctx, runID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !completed {
		t.Fatal("expected completed")
	}
	if result != `{"custom":"result"}` {
		t.Fatalf("expected custom result, got %q", result)
	}
}

func TestInMemoryChildWorkflowStore_Handler(t *testing.T) {
	s := NewInMemoryChildWorkflowStore()
	ctx := context.Background()

	s.SetHandler("child-workflow", func(inputJSON string) (string, error) {
		return `{"handler":"executed","input":` + inputJSON + `}`, nil
	})

	runID, err := s.StartChildWorkflow(ctx, "parent-1", "child-workflow", `{"x":1}`, 0, "")
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	result, completed, err := s.GetChildResult(ctx, runID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !completed {
		t.Fatal("expected completed")
	}
	if result != `{"handler":"executed","input":{"x":1}}` {
		t.Fatalf("expected handler result, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// InMemoryConcurrencyKeyStore tests
// ---------------------------------------------------------------------------

func TestInMemoryConcurrencyKeyStore_AcquireAndRelease(t *testing.T) {
	s := NewInMemoryConcurrencyKeyStore()
	ctx := context.Background()

	acquired, err := s.AcquireConcurrencyKey(ctx, "key-1", "wf-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquired")
	}

	// Same workflow should re-acquire.
	acquired, err = s.AcquireConcurrencyKey(ctx, "key-1", "wf-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected re-acquire by same workflow")
	}

	// Different workflow should fail.
	acquired, err = s.AcquireConcurrencyKey(ctx, "key-1", "wf-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Fatal("expected not acquired by different workflow")
	}

	// Release.
	if err := s.ReleaseConcurrencyKey(ctx, "key-1"); err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}

	// Now wf-2 should be able to acquire.
	acquired, err = s.AcquireConcurrencyKey(ctx, "key-1", "wf-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquired after release")
	}
}

func TestInMemoryConcurrencyKeyStore_Expiry(t *testing.T) {
	s := NewInMemoryConcurrencyKeyStore()
	ctx := context.Background()

	// Acquire with zero TTL (effectively expired).
	acquired, err := s.AcquireConcurrencyKey(ctx, "key-1", "wf-1", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquired")
	}

	// Different workflow should be able to acquire since the key is expired.
	acquired, err = s.AcquireConcurrencyKey(ctx, "key-1", "wf-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquired after expiry")
	}
}

// ---------------------------------------------------------------------------
// WasmTestEnv tests
// ---------------------------------------------------------------------------

func TestNewWasmTestEnv(t *testing.T) {
	env := NewWasmTestEnv(t)
	defer env.Close()

	if env.engine == nil {
		t.Fatal("expected non-nil engine")
	}
	if env.SignalStore == nil {
		t.Fatal("expected non-nil SignalStore")
	}
	if env.PromiseStore == nil {
		t.Fatal("expected non-nil PromiseStore")
	}
	if env.ChildWorkflowStore == nil {
		t.Fatal("expected non-nil ChildWorkflowStore")
	}
	if env.ConcurrencyStore == nil {
		t.Fatal("expected non-nil ConcurrencyStore")
	}
	if env.WorkflowState == nil {
		t.Fatal("expected non-nil WorkflowState")
	}
	if env.WorkflowID == "" {
		t.Fatal("expected non-empty WorkflowID")
	}
}

func TestWasmTestEnv_Options(t *testing.T) {
	env := NewWasmTestEnv(t,
		WithWorkflowID("custom-id"),
		WithDefName("custom-def"),
		WithDefVersion(42),
	)
	defer env.Close()

	if env.WorkflowID != "custom-id" {
		t.Fatalf("expected WorkflowID custom-id, got %q", env.WorkflowID)
	}
	if env.DefName != "custom-def" {
		t.Fatalf("expected DefName custom-def, got %q", env.DefName)
	}
	if env.DefVersion != 42 {
		t.Fatalf("expected DefVersion 42, got %d", env.DefVersion)
	}
}

func TestWasmTestEnv_CallerRecordsCalls(t *testing.T) {
	env := NewWasmTestEnv(t)
	defer env.Close()

	caller := env.Caller()
	if caller == nil {
		t.Fatal("expected non-nil caller")
	}

	// Make a direct call through the caller.
	resp, err := caller.Call(context.Background(), "test-svc", "test-op", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp == "" {
		t.Fatal("expected non-empty response")
	}

	if len(caller.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.Calls))
	}
	if caller.Calls[0].Service != "test-svc" {
		t.Fatalf("expected service test-svc, got %q", caller.Calls[0].Service)
	}
	if caller.Calls[0].Op != "test-op" {
		t.Fatalf("expected op test-op, got %q", caller.Calls[0].Op)
	}
}

func TestDefaultResponse(t *testing.T) {
	tests := []struct {
		service   string
		operation string
		want      string
	}{
		{"catalog", "LookupItem", `{"sku":"ABC-123","name":"Widget","price_cents":999,"found":true}`},
		{"inventory", "Reserve", `{"reservation_id":"resv_abc123","status":"reserved","total_cents":3299}`},
		{"payments", "Charge", `{"charge_id":"chg_xyz789","status":"captured"}`},
		{"shipping", "CreateShipment", `{"tracking_id":"TRACK-123456","status":"label_created"}`},
		{"unknown", "Op", `{}`},
	}

	for _, tt := range tests {
		got := defaultResponse(tt.service, tt.operation)
		if got != tt.want {
			t.Errorf("defaultResponse(%q, %q) = %q, want %q", tt.service, tt.operation, got, tt.want)
		}
	}
}

func TestWorkflowStateType(t *testing.T) {
	ws := NewTestWorkflowState()
	if ws.Version() != 1 {
		t.Fatalf("expected version 1, got %d", ws.Version())
	}
	if ws.MinVersion() != 1 {
		t.Fatalf("expected minVersion 1, got %d", ws.MinVersion())
	}

	// Child version.
	v, ok := ws.ChildVersion("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
	if v != 0 {
		t.Fatalf("expected version 0, got %d", v)
	}

	ws.ChildVersions["child-a"] = 3
	v, ok = ws.ChildVersion("child-a")
	if !ok {
		t.Fatal("expected found")
	}
	if v != 3 {
		t.Fatalf("expected version 3, got %d", v)
	}
}
