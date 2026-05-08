package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/host/testutil"
)

// ---------------------------------------------------------------------------
// Mock types for engine-level fault injection tests
// ---------------------------------------------------------------------------

// errorCaller is a mock ServiceCaller that can inject errors on specific
// service/operation combinations. When the configured service matches, it
// returns an error for failCount consecutive calls, then falls through to the
// normal mockResponse helper.
type errorCaller struct {
	// failService is the service name whose calls should fail (e.g. "catalog").
	// An empty string disables injection.
	failService string
	// failOp is an optional operation filter.  If non-empty, only the specific
	// operation on failService is made to fail.  If empty, all operations on
	// failService fail.
	failOp string
	// failCount is how many matching calls should return an error before the
	// caller begins succeeding again.
	failCount int

	// records records every call made through this caller for test assertions.
	records []CallRecord
}

func (c *errorCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	rec := CallRecord{
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
	}
	shouldFail := service == c.failService && (c.failOp == "" || operation == c.failOp)
	if shouldFail && c.failCount > 0 {
		c.failCount--
		rec.Err = fmt.Sprintf("injected fault on %s.%s", service, operation)
		c.records = append(c.records, rec)
		return "", fmt.Errorf("%s", rec.Err)
	}
	resp := mockResponse(service, operation)
	rec.Response = resp
	c.records = append(c.records, rec)
	return resp, nil
}

// retryableErr implements RetryableError with a configurable retryable flag so
// that tests can exercise both the retryable and non-retryable code paths in
// isDefinitelyNonRetryable.
type retryableErr struct {
	msg       string
	retryable bool
}

func (e *retryableErr) Error() string        { return e.msg }
func (e *retryableErr) Retryable() bool       { return e.retryable }

// ---------------------------------------------------------------------------
// Test 1: Engine handles DurableCall error gracefully
// ---------------------------------------------------------------------------

// TestFaultInjectedCallError verifies that the engine does not panic or hang
// when a DurableCall returns an error from the ServiceCaller.  The error must
// be captured either in the returned error or in the event history, and the
// engine must remain responsive.
func TestFaultInjectedCallError(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })

	// Make the caller fail on the very first service call (catalog.LookupItem).
	caller := &errorCaller{
		failService: "catalog",
		failCount:   1,
	}
	engine := NewEngine(rt, caller)
	input := []byte(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)

	done := make(chan struct{})
	var result string
	var history []EventRecord
	var execErr error
	go func() {
		defer close(done)
		result, history, _, _, _, execErr = engine.Execute(ctx, wasmBytes, "place_order", input)
	}()

	select {
	case <-done:
		t.Logf("Execute returned: result=%q, err=%v, history=%d events", result, execErr, len(history))

		// The engine must not have panicked (we completed normally).
		// Verify that either an error was returned or the failure was recorded
		// in the event history.
		if execErr == nil && len(history) == 0 {
			t.Error("expected either an error from Execute or events in history")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Engine.Execute hung when service call returned an error")
	}
}

// ---------------------------------------------------------------------------
// Test 2: Retry classification logic
// ---------------------------------------------------------------------------

// TestFaultNonRetryableClassification tests isDefinitelyNonRetryable, the
// function that determines whether a DurableCallWithRetry should stop retrying
// or continue.  It covers the RetryableError interface, substring matching on
// error messages, and the interaction between the two.
func TestFaultNonRetryableClassification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		patterns []string
		want     bool
	}{
		{
			name: "plain error, no patterns",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name:     "plain error, matching pattern",
			err:      errors.New("disk full"),
			patterns: []string{"disk full"},
			want:     true,
		},
		{
			name:     "plain error, non-matching pattern",
			err:      errors.New("timeout"),
			patterns: []string{"disk full"},
			want:     false,
		},
		{
			name:     "plain error, empty patterns",
			err:      errors.New("any error"),
			patterns: []string{},
			want:     false,
		},
		{
			name: "non-retryable via interface",
			err:  &retryableErr{msg: "fatal", retryable: false},
			want: true,
		},
		{
			name: "retryable via interface",
			err:  &retryableErr{msg: "transient", retryable: true},
			want: false,
		},
		{
			name:     "non-retryable interface with patterns",
			err:      &retryableErr{msg: "fatal err", retryable: false},
			patterns: []string{"disk full"},
			want:     true,
		},
		{
			name:     "error message contains pattern substring",
			err:      errors.New("connection refused: deadline exceeded"),
			patterns: []string{"deadline exceeded"},
			want:     true,
		},
		{
			name:     "multiple patterns, last one matches",
			err:      errors.New("disk quota exceeded"),
			patterns: []string{"timeout", "disk quota exceeded", "broken pipe"},
			want:     true,
		},
		{
			name:     "multiple patterns, none match",
			err:      errors.New("unknown error"),
			patterns: []string{"timeout", "disk full"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDefinitelyNonRetryable(tt.err, tt.patterns)
			if got != tt.want {
				t.Errorf("isDefinitelyNonRetryable(%v, %v) = %v, want %v",
					tt.err, tt.patterns, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 3: Event persistence survives DB write failure
// ---------------------------------------------------------------------------

// TestFaultEventPersistenceWithDBError verifies that a database write failure
// during AppendEventHistoryBatch does not corrupt or erase events that were
// previously committed.  The FaultInjector simulates a network partition by
// returning a cancelled context; attempts to write with that context fail at
// the transaction level, leaving earlier events intact.
func TestFaultEventPersistenceWithDBError(t *testing.T) {
	db := testutil.TestDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	fi := NewFaultInjector(db)
	t.Cleanup(func() { fi.Cleanup() })
	ctx := context.Background()

	runID := fmt.Sprintf("test-fault-persist-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})

	// Append first batch of events successfully.
	firstEvents := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2", Request: `{}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, firstEvents); err != nil {
		t.Fatalf("First append: %v", err)
	}
	t.Log("First batch appended successfully")

	// Inject a DB error via the FaultInjector's cancelled context.  The next
	// write should fail because BeginTx sees the cancelled context.
	fi.InjectNetworkPartition()
	secondEvents := []EventRecord{
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op3", Request: `{}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(fi.Context(ctx), runID, secondEvents); err == nil {
		t.Error("Expected error on append during simulated DB failure, got nil")
	} else {
		t.Logf("Second append correctly failed: %v", err)
	}
	fi.Cleanup()

	// Load event history — the first batch must be intact.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("Load history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("Expected 2 events (first batch), got %d", len(history))
	}
	if len(history) >= 1 && history[0].Op != "op1" {
		t.Errorf("Expected op1 as first event, got %s", history[0].Op)
	}
	if len(history) >= 2 && history[1].Op != "op2" {
		t.Errorf("Expected op2 as second event, got %s", history[1].Op)
	}

	// Optionally confirm that step 2 (the failed append) is NOT present.
	for _, ev := range history {
		if ev.Step == 2 {
			t.Errorf("Step 2 should not be present (its append failed)")
		}
	}
}

// ---------------------------------------------------------------------------
// Test 4: ClaimWorkflow returns nil on DB error
// ---------------------------------------------------------------------------

// TestFaultClaimWithDBError verifies that ClaimWorkflow returns (nil, error)
// when the database is unreachable, rather than panicking or blocking.  The
// FaultInjector simulates unavailability by cancelling the context passed to
// the store.  This mirrors what a real worker sees during a network partition.
func TestFaultClaimWithDBError(t *testing.T) {
	db := testutil.TestDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	fi := NewFaultInjector(db)
	t.Cleanup(func() { fi.Cleanup() })
	ctx := context.Background()

	// Inject a DB failure via FaultInjector's cancelled context.
	fi.InjectNetworkPartition()

	// ClaimWorkflow should not panic; it should return nil with a non-nil
	// error describing the failure.
	wf, err := store.ClaimWorkflow(fi.Context(ctx), "test-worker", "default")
	if wf != nil {
		t.Error("ClaimWorkflow should return nil when DB is unavailable")
	}
	if err == nil {
		t.Error("ClaimWorkflow should return a non-nil error when DB is unavailable")
	} else {
		t.Logf("ClaimWorkflow returned expected error: %v", err)
	}

	// Restore connectivity so subsequent tests are not affected.
	fi.Cleanup()
}

// ---------------------------------------------------------------------------
// Test 5: Fault injector on/off toggle
// ---------------------------------------------------------------------------

// TestFaultInjectorToggle proves that the FaultInjector is actually
// responsible for the failures by toggling a fault on and then off.  When the
// fault is active, store operations fail.  After Cleanup, the same operation
// succeeds, confirming that the injector is the cause.
func TestFaultInjectorToggle(t *testing.T) {
	db := testutil.TestDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	fi := NewFaultInjector(db)
	t.Cleanup(func() { fi.Cleanup() })
	ctx := context.Background()

	runID := fmt.Sprintf("test-fault-toggle-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})

	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
	}

	// ---- Phase 1: Fault active — operation must fail ----
	fi.InjectNetworkPartition()
	if err := store.AppendEventHistoryBatch(fi.Context(ctx), runID, events); err == nil {
		t.Error("Expected append failure during active network partition")
	} else {
		t.Logf("Append correctly failed during partition: %v", err)
	}
	if !fi.IsActive(FaultNetworkPartition) {
		t.Error("FaultNetworkPartition should be active after InjectNetworkPartition")
	}

	// ---- Phase 2: Fault cleared — same operation must succeed ----
	fi.Cleanup()
	if fi.IsActive(FaultNetworkPartition) {
		t.Error("FaultNetworkPartition should be inactive after Cleanup")
	}
	if err := store.AppendEventHistoryBatch(fi.Context(ctx), runID, events); err != nil {
		t.Fatalf("Append failed after partition cleanup: %v", err)
	}
	t.Log("Append succeeded after partition cleanup")

	// Confirm the events actually landed.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("Load history: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("Expected 1 event after successful append, got %d", len(history))
	}
}
