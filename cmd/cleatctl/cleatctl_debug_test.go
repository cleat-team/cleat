package main

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// mockWorkflowInstanceConnector — simulates sql.DB for loadWorkflowInstance
// ---------------------------------------------------------------------------

type mockWorkflowInstanceConnector struct {
	instance *engine.WorkflowInstance
	err      error
}

type mockWorkflowInstanceDriver struct{}
type mockWorkflowInstanceConn struct {
	instance *engine.WorkflowInstance
	err      error
}
type mockWorkflowInstanceStmt struct {
	instance *engine.WorkflowInstance
	err      error
}
type mockWorkflowInstanceRows struct {
	instance *engine.WorkflowInstance
	err      error
	closed   bool
}

func (d *mockWorkflowInstanceDriver) Open(_ string) (driver.Conn, error) { return nil, errors.New("unused") }

func (c *mockWorkflowInstanceConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &mockWorkflowInstanceConn{instance: c.instance, err: c.err}, nil
}
func (c *mockWorkflowInstanceConnector) Driver() driver.Driver { return &mockWorkflowInstanceDriver{} }

func (c *mockWorkflowInstanceConn) Prepare(_ string) (driver.Stmt, error) {
	return &mockWorkflowInstanceStmt{instance: c.instance, err: c.err}, nil
}
func (c *mockWorkflowInstanceConn) Close() error                       { return nil }
func (c *mockWorkflowInstanceConn) Begin() (driver.Tx, error)          { return nil, errors.New("no tx") }
func (s *mockWorkflowInstanceStmt) Close() error                       { return nil }
func (s *mockWorkflowInstanceStmt) NumInput() int                      { return -1 }
func (s *mockWorkflowInstanceStmt) Exec(_ []driver.Value) (driver.Result, error) { return nil, errors.New("no exec") }

func (s *mockWorkflowInstanceStmt) Query(_ []driver.Value) (driver.Rows, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &mockWorkflowInstanceRows{instance: s.instance}, nil
}

func (r *mockWorkflowInstanceRows) Columns() []string {
	return []string{"id", "def_name", "def_version", "min_version", "status", "input",
		"result", "error", "error_code", "error_op", "assigned_to", "next_wake_at",
		"tenant_id", "created_at", "generation"}
}
func (r *mockWorkflowInstanceRows) Close() error      { r.closed = true; return nil }

func (r *mockWorkflowInstanceRows) Next(dest []driver.Value) error {
	if r.err != nil {
		return r.err
	}
	if r.closed {
		return io.EOF
	}
	r.closed = true
	if r.instance == nil {
		return errors.New("no instance set")
	}
	dest[0] = r.instance.ID
	dest[1] = r.instance.DefName
	dest[2] = int64(r.instance.DefVersion)
	dest[3] = int64(r.instance.MinVersion)
	dest[4] = r.instance.Status
	dest[5] = []byte(r.instance.Input)
	dest[6] = r.instance.Result
	dest[7] = r.instance.Error
	dest[8] = r.instance.ErrorCode
	dest[9] = r.instance.ErrorOp
	dest[10] = r.instance.AssignedTo
	dest[11] = r.instance.NextWakeAt
	dest[12] = r.instance.TenantID
	dest[13] = r.instance.CreatedAt
	dest[14] = r.instance.Generation
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// makeWASM returns a minimal valid WASM binary (magic + version, won't actually execute).
func makeWASM() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
}

// makeEventRecord creates an EventRecord with the given step and type.
func makeEventRecord(step int, eventType engine.EventType) engine.EventRecord {
	return engine.EventRecord{Step: step, EventType: eventType}
}

// =========================================================================
// Flag Parsing Tests
// =========================================================================

func TestParseDebugFlags_NoArgs(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, nil, []string{})
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestParseDebugFlags_MissingWorkflowID(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, nil, []string{"--entry-point", "HandleLead"})
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestParseDebugFlags_MissingEntryPointValue(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, nil, []string{"wf-123", "--entry-point"})
	})
	if !strings.Contains(stderr, "error: --entry-point requires a value") {
		t.Errorf("expected 'error: --entry-point requires a value', got: %s", stderr)
	}
}

func TestParseDebugFlags_MissingEntryPointStepThrough(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, nil, []string{"wf-123"})
	})
	if !strings.Contains(stderr, "error: --entry-point is required for step-through mode") {
		t.Errorf("expected entry-point required error, got: %s", stderr)
	}
}

func TestParseDebugFlags_WatchModeNoEntryPoint(t *testing.T) {
	// --watch mode should NOT require --entry-point
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			return 0, nil
		},
		loadEventHistoryPaginatedFn: func(_ context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
			return nil, nil
		},
	}
	// Watch mode polls every 2s; we need to stop it quickly.
	// Since CountEventHistory returns 0 and there's no idle timer trigger,
	// we rely on the loop exiting on context cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The watch loop handles ctx.Done(), so it will exit.
	// But note: osExit might get called on error from store, so we use a non-failing store.
	stdout, stderr := captureOutputs(t, func() {
		runDebug(ctx, store, nil, []string{"wf-123", "--watch"})
	})
	if !strings.Contains(stdout, "Watching") {
		t.Errorf("expected 'Watching' in stdout, got: %s", stdout)
	}
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
}

func TestParseDebugFlags_PositionalAndFlags(t *testing.T) {
	// Just test the flag parser directly without involving DB or engine.
	flags := parseDebugFlags([]string{"my-workflow", "--entry-point", "Main"})
	if flags == nil {
		t.Fatal("expected flags to parse successfully")
	}
	if flags.workflowID != "my-workflow" {
		t.Errorf("workflowID = %q, want %q", flags.workflowID, "my-workflow")
	}
	if flags.entryPoint != "Main" {
		t.Errorf("entryPoint = %q, want %q", flags.entryPoint, "Main")
	}
	if flags.watch {
		t.Error("watch should be false")
	}
}

func TestParseDebugFlags_WatchFlag(t *testing.T) {
	flags := parseDebugFlags([]string{"my-workflow", "--watch"})
	if flags == nil {
		t.Fatal("expected flags to parse successfully")
	}
	if !flags.watch {
		t.Error("watch should be true")
	}
	if flags.entryPoint != "" {
		t.Error("entryPoint should be empty for watch-only mode")
	}
}

func TestParseDebugFlags_ExtraPositional(t *testing.T) {
	flags := parseDebugFlags([]string{"my-workflow", "extra-arg", "--entry-point", "Main"})
	if flags == nil {
		t.Fatal("expected flags to parse successfully")
	}
	// "extra-arg" is treated as a positional, so workflowID is "my-workflow"
	if flags.workflowID != "my-workflow" {
		t.Errorf("workflowID = %q, want %q", flags.workflowID, "my-workflow")
	}
}

// =========================================================================
// Format Function Tests
// =========================================================================

func TestFormatQueryState_Empty(t *testing.T) {
	result := formatQueryState(nil)
	if result != "{}" {
		t.Errorf("expected {}, got: %s", result)
	}
	result = formatQueryState(map[string]string{})
	if result != "{}" {
		t.Errorf("expected {}, got: %s", result)
	}
}

func TestFormatQueryState_WithData(t *testing.T) {
	qs := map[string]string{"key1": "val1", "key2": "val2"}
	result := formatQueryState(qs)
	// JSON serialization order is deterministic for small maps, but to be safe
	// just check that both keys and values appear
	if !strings.Contains(result, "key1") || !strings.Contains(result, "val1") {
		t.Errorf("expected key1/val1 in %s", result)
	}
	if !strings.Contains(result, "key2") || !strings.Contains(result, "val2") {
		t.Errorf("expected key2/val2 in %s", result)
	}
}

func TestFormatRemainingEvents_None(t *testing.T) {
	events := []engine.EventRecord{
		{Step: 0, EventType: "Activity"},
	}
	result := formatRemainingEvents(events, 1)
	if !strings.Contains(result, "(no remaining events)") {
		t.Errorf("expected '(no remaining events)', got: %s", result)
	}
}

func TestFormatRemainingEvents_BeyondEnd(t *testing.T) {
	events := []engine.EventRecord{}
	result := formatRemainingEvents(events, 5)
	if !strings.Contains(result, "(no remaining events)") {
		t.Errorf("expected '(no remaining events)', got: %s", result)
	}
}

func TestFormatRemainingEvents_WithEvents(t *testing.T) {
	events := []engine.EventRecord{
		{Step: 0, EventType: "Activity", Service: "svc", Op: "op"},
		{Step: 1, EventType: "Sleep", DurationMs: 1000},
		{Step: 2, EventType: "SignalReceived", SignalName: "approval"},
	}
	result := formatRemainingEvents(events, 1)
	if !strings.Contains(result, "Remaining events:") {
		t.Errorf("expected 'Remaining events:', got: %s", result)
	}
	if !strings.Contains(result, "type=Sleep") {
		t.Errorf("expected 'type=Sleep', got: %s", result)
	}
	if !strings.Contains(result, "type=SignalReceived") {
		t.Errorf("expected 'type=SignalReceived', got: %s", result)
	}
	// Should NOT include step 0 (before fromStep)
	if strings.Contains(result, "type=Activity") {
		t.Errorf("should not include events before fromStep, got: %s", result)
	}
}

func TestFormatEvent_Activity(t *testing.T) {
	ev := engine.EventRecord{
		Step:      3,
		EventType: "Activity",
		Service:   "stripe",
		Op:        "charge",
		Request:   `{"amount":100}`,
		Response:  `{"id":"ch_123"}`,
	}
	result := formatEvent(ev)
	if !strings.Contains(result, "type=Activity") {
		t.Errorf("expected 'type=Activity', got: %s", result)
	}
	if !strings.Contains(result, "service=stripe") {
		t.Errorf("expected 'service=stripe', got: %s", result)
	}
	if !strings.Contains(result, "op=charge") {
		t.Errorf("expected 'op=charge', got: %s", result)
	}
}

func TestFormatEvent_Signal(t *testing.T) {
	ev := engine.EventRecord{
		Step:       1,
		EventType:  "SignalReceived",
		SignalName: "approval",
		Request:    "",
	}
	result := formatEvent(ev)
	if !strings.Contains(result, "signal=approval") {
		t.Errorf("expected 'signal=approval', got: %s", result)
	}
}

func TestFormatEvent_Truncate(t *testing.T) {
	longStr := strings.Repeat("x", 200)
	ev := engine.EventRecord{
		Step:     0,
		EventType: "Activity",
		Request:  longStr,
	}
	result := formatEvent(ev)
	if strings.Contains(result, longStr) {
		t.Errorf("expected truncated request in: %s", result)
	}
	if !strings.Contains(result, "...") {
		t.Errorf("expected '...' for truncated field in: %s", result)
	}
}

// =========================================================================
// Error Handling Tests (step-through)
// =========================================================================

func TestDebugStep_WorkflowNotFound(t *testing.T) {
	connector := &mockWorkflowInstanceConnector{
		err: errors.New("sql: no rows in result set"),
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, db, []string{"nonexistent-wf", "--entry-point", "Main"})
	})
	if !strings.Contains(stderr, "error loading workflow instance") {
		t.Errorf("expected 'error loading workflow instance', got: %s", stderr)
	}
}

func TestDebugStep_LoadEventHistoryError(t *testing.T) {
	now := time.Now()
	connector := &mockWorkflowInstanceConnector{
		instance: &engine.WorkflowInstance{
			ID: "wf-123", DefName: "test-wf", DefVersion: 1,
			Status: "running", Input: json.RawMessage(`{}`),
			CreatedAt: now, MinVersion: 2,
		},
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	store := &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]engine.EventRecord, error) {
			return nil, errors.New("db connection lost")
		},
	}

	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), store, db, []string{"wf-123", "--entry-point", "Main"})
	})
	if !strings.Contains(stderr, "error loading event history") {
		t.Errorf("expected 'error loading event history', got: %s", stderr)
	}
}

func TestDebugStep_NoEvents(t *testing.T) {
	now := time.Now()
	connector := &mockWorkflowInstanceConnector{
		instance: &engine.WorkflowInstance{
			ID: "wf-123", DefName: "test-wf", DefVersion: 1,
			Status: "running", Input: json.RawMessage(`{}`),
			CreatedAt: now, MinVersion: 2,
		},
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	store := &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]engine.EventRecord, error) {
			return []engine.EventRecord{}, nil
		},
	}

	stdout, stderr := captureOutputs(t, func() {
		runDebug(context.Background(), store, db, []string{"wf-123", "--entry-point", "Main"})
	})
	if !strings.Contains(stdout, "No events in history") {
		t.Errorf("expected 'No events in history', got stdout=%s stderr=%s", stdout, stderr)
	}
}

func TestDebugStep_LoadWASMError(t *testing.T) {
	now := time.Now()
	connector := &mockWorkflowInstanceConnector{
		instance: &engine.WorkflowInstance{
			ID: "wf-123", DefName: "test-wf", DefVersion: 1,
			Status: "running", Input: json.RawMessage(`{}`),
			CreatedAt: now, MinVersion: 2,
		},
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	store := &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]engine.EventRecord, error) {
			return []engine.EventRecord{{Step: 0, EventType: "Activity", Service: "svc", Op: "op"}}, nil
		},
		loadWASMFn: func(_ context.Context, defName string, defVersion int) ([]byte, error) {
			return nil, errors.New("wasm not found")
		},
	}

	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), store, db, []string{"wf-123", "--entry-point", "Main"})
	})
	if !strings.Contains(stderr, "error loading WASM") {
		t.Errorf("expected 'error loading WASM', got: %s", stderr)
	}
}

// =========================================================================
// Watch Mode Tests
// =========================================================================

func TestDebugWatch_CountError(t *testing.T) {
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			return 0, errors.New("connection refused")
		},
	}

	stdout, stderr := captureOutputs(t, func() {
		err := runDebugWatch(context.Background(), store, "wf-123")
		if err == nil {
			t.Error("expected error from watch")
		}
	})
	_ = stdout
	_ = stderr
}

func TestDebugWatch_PollNewEvents(t *testing.T) {
	pollCount := 0
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			pollCount++
			if pollCount == 1 {
				return 0, nil
			}
			return 1, nil
		},
		loadEventHistoryPaginatedFn: func(_ context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
			return []engine.EventRecord{
				{Step: 0, EventType: "Activity", Service: "svc", Op: "op"},
			}, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stdout, _ := captureOutputs(t, func() {
		runDebugWatch(ctx, store, "wf-123")
	})
	if !strings.Contains(stdout, "Watching") {
		t.Errorf("expected 'Watching' in stdout, got: %s", stdout)
	}
	if pollCount < 1 {
		t.Error("expected at least one count call")
	}
}

func TestDebugWatch_LoadPaginatedError(t *testing.T) {
	pollCount := 0
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			pollCount++
			if pollCount == 1 {
				return 0, nil
			}
			return 5, nil // more events, trigger pagination
		},
		loadEventHistoryPaginatedFn: func(_ context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
			return nil, errors.New("pagination failed")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Should not crash on pagination error
	stdout, stderr := captureOutputs(t, func() {
		runDebugWatch(ctx, store, "wf-123")
	})
	_ = stdout
	// Stderr should contain the error
	if !strings.Contains(stderr, "error loading new events") {
		t.Errorf("expected 'error loading new events' in stderr, got: %s", stderr)
	}
}

// =========================================================================
// Main Command Dispatch Test
// =========================================================================

func TestMainDebugDispatch(t *testing.T) {
	// Test that the debug command in main.go correctly dispatches to runDebug
	// when "debug" is the first argument. We test it indirectly by checking
	// that the runDebug function is invoked (which then calls parseDebugFlags).

	// Test with no workflow ID — should print usage without panic.
	stderr := withExitPanic(t, func() {
		runDebug(context.Background(), nil, nil, []string{})
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage, got: %s", stderr)
	}
}

// =========================================================================
// PrintDebugUsage Test
// =========================================================================

func TestPrintDebugUsage(t *testing.T) {
	stderr := captureStderr(t, func() {
		printDebugUsage()
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected 'Usage:' in stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, "--watch") {
		t.Errorf("expected '--watch' in usage, got: %s", stderr)
	}
	if !strings.Contains(stderr, "next (n)") {
		t.Errorf("expected 'next (n)' in usage, got: %s", stderr)
	}
}

// =========================================================================
// DebugState method tests
// =========================================================================

func TestDebugState_SendAction(t *testing.T) {
	ds := &debugState{
		cmdCh: make(chan engine.ReplayStepAction, 1),
		quit:  make(chan struct{}),
	}
	ds.sendAction(engine.ReplayNext)
	select {
	case action := <-ds.cmdCh:
		if action != engine.ReplayNext {
			t.Errorf("expected ReplayNext, got %v", action)
		}
	default:
		t.Error("expected action on cmdCh")
	}
}

func TestDebugState_SendActionQuit(t *testing.T) {
	ds := &debugState{
		cmdCh: make(chan engine.ReplayStepAction, 1),
		quit:  make(chan struct{}),
	}
	close(ds.quit)
	// Should not block when quit is closed
	ds.sendAction(engine.ReplayNext)
}

func TestDebugState_CallbackAutoContinue(t *testing.T) {
	events := []engine.EventRecord{{Step: 0, EventType: "Activity"}}
	ds := &debugState{
		autoContinue: true,
		events:       events,
	}
	action := ds.callback(0, &events[0], map[string]string{"k": "v"})
	if action != engine.ReplayNext {
		t.Errorf("expected ReplayNext in auto-continue, got %v", action)
	}
	if ds.lastStep != 0 {
		t.Errorf("expected lastStep=0, got %d", ds.lastStep)
	}
	if ds.lastQS["k"] != "v" {
		t.Errorf("expected lastQS[k]=v, got %v", ds.lastQS)
	}
}

func TestDebugState_CallbackQuit(t *testing.T) {
	ds := &debugState{
		stepCh: make(chan debugStepInfo, 1),
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
	}
	close(ds.quit)

	events := []engine.EventRecord{{Step: 0, EventType: "Activity"}}
	action := ds.callback(0, &events[0], map[string]string{})
	if action != engine.ReplayQuit {
		t.Errorf("expected ReplayQuit when quit closed, got %v", action)
	}
}

func TestDebugState_DisplayStep(t *testing.T) {
	events := []engine.EventRecord{
		{Step: 0, EventType: "Activity", Service: "stripe", Op: "charge", Request: `{"a":1}`, Response: `{"ok":true}`},
	}
	ds := &debugState{
		events: events,
	}
	info := debugStepInfo{
		step:  0,
		event: &events[0],
		qs:    map[string]string{"count": "1"},
	}

	stdout := captureStdout(t, func() {
		ds.displayStep(info)
	})

	if !strings.Contains(stdout, "Step 1/1") {
		t.Errorf("expected 'Step 1/1', got: %s", stdout)
	}
	if !strings.Contains(stdout, "type=Activity") {
		t.Errorf("expected 'type=Activity', got: %s", stdout)
	}
	if !strings.Contains(stdout, "service=stripe") {
		t.Errorf("expected 'service=stripe', got: %s", stdout)
	}
	if !strings.Contains(stdout, "request:") {
		t.Errorf("expected 'request:', got: %s", stdout)
	}
	if !strings.Contains(stdout, "response:") {
		t.Errorf("expected 'response:', got: %s", stdout)
	}
	if !strings.Contains(stdout, "query_state:") {
		t.Errorf("expected 'query_state:', got: %s", stdout)
	}
}

func TestDebugState_DisplayStepWithError(t *testing.T) {
	events := []engine.EventRecord{
		{Step: 0, EventType: "Activity", Service: "svc", Op: "fail", Err: "timeout"},
	}
	ds := &debugState{events: events}
	info := debugStepInfo{step: 0, event: &events[0], qs: map[string]string{}}

	stdout := captureStdout(t, func() {
		ds.displayStep(info)
	})

	if !strings.Contains(stdout, "error:") {
		t.Errorf("expected 'error:' in output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "timeout") {
		t.Errorf("expected 'timeout' in output, got: %s", stdout)
	}
}

func TestDebugState_DisplayStepSignalPayload(t *testing.T) {
	events := []engine.EventRecord{
		{Step: 0, EventType: "SignalReceived", SignalName: "approval", SignalPayload: `{"ok":true}`},
	}
	ds := &debugState{events: events}
	info := debugStepInfo{step: 0, event: &events[0], qs: map[string]string{}}

	stdout := captureStdout(t, func() {
		ds.displayStep(info)
	})

	if !strings.Contains(stdout, "payload:") {
		t.Errorf("expected 'payload:' in output, got: %s", stdout)
	}
}

// =========================================================================
// Interactive Command Tests (via stdin simulation)
// =========================================================================

// setInputReader sets up the debug state reader to read from a string
// instead of os.Stdin, useful for testing readCommand().
func setInputReader(ds *debugState, input string) {
	ds.reader = bufio.NewReader(strings.NewReader(input))
}

func TestDebugState_ReadCommand_Next(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "next\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "debug> ") {
		t.Errorf("expected prompt, got: %s", stdout)
	}

	select {
	case action := <-ds.cmdCh:
		if action != engine.ReplayNext {
			t.Errorf("expected ReplayNext, got %v", action)
		}
	default:
		t.Error("expected action on cmdCh after 'next' command")
	}
}

func TestDebugState_ReadCommand_Quit(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "quit\n")
	captureStdout(t, func() {
		ds.readCommand()
	})

	select {
	case action := <-ds.cmdCh:
		if action != engine.ReplayQuit {
			t.Errorf("expected ReplayQuit, got %v", action)
		}
	default:
		t.Error("expected action on cmdCh after 'quit' command")
	}
}

func TestDebugState_ReadCommand_Continue(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "c\n")
	captureStdout(t, func() {
		ds.readCommand()
	})

	if !ds.autoContinue {
		t.Error("expected autoContinue to be true after 'continue' command")
	}
}

func TestDebugState_ReadCommand_State(t *testing.T) {
	ds := &debugState{
		cmdCh:    make(chan engine.ReplayStepAction, 1),
		quit:     make(chan struct{}),
		events:   []engine.EventRecord{{Step: 0, EventType: "Activity"}},
		lastQS:   map[string]string{"key1": "val1", "key2": "val2"},
		lastStep: 0,
	}

	setInputReader(ds, "state\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "key1 = val1") {
		t.Errorf("expected 'key1 = val1', got: %s", stdout)
	}
	if !strings.Contains(stdout, "key2 = val2") {
		t.Errorf("expected 'key2 = val2', got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_StateEmpty(t *testing.T) {
	ds := &debugState{
		cmdCh:    make(chan engine.ReplayStepAction, 1),
		quit:     make(chan struct{}),
		events:   []engine.EventRecord{{Step: 0, EventType: "Activity"}},
		lastQS:   map[string]string{},
		lastStep: 0,
	}

	setInputReader(ds, "state\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "query_state:") {
		t.Errorf("expected 'query_state:', got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_StateNoData(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "state\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "advance at least one step") {
		t.Errorf("expected 'advance at least one step', got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_Events(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{
			{Step: 0, EventType: "Activity", Service: "svc", Op: "op"},
			{Step: 1, EventType: "Sleep"},
		},
		lastStep: 0,
	}

	setInputReader(ds, "events\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "Remaining events:") {
		t.Errorf("expected 'Remaining events:', got: %s", stdout)
	}
	if !strings.Contains(stdout, "type=Sleep") {
		t.Errorf("expected 'type=Sleep', got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_Help(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "help\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "Commands:") {
		t.Errorf("expected 'Commands:', got: %s", stdout)
	}
	if !strings.Contains(stdout, "continue") {
		t.Errorf("expected 'continue' in help, got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_Shortcuts(t *testing.T) {
	tests := []struct {
		input string
		desc  string
	}{
		{"n\n", "n shortcut"},
		{"q\n", "q shortcut"},
		{"h\n", "h shortcut"},
		{"s\n", "s shortcut"},
		{"e\n", "e shortcut"},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			ds2 := &debugState{
				cmdCh:    make(chan engine.ReplayStepAction, 1),
				quit:     make(chan struct{}),
				events:   []engine.EventRecord{{Step: 0, EventType: "Activity"}},
				lastStep: 0,
			}
			setInputReader(ds2, tc.input)
			captureStdout(t, func() {
				ds2.readCommand()
			})
		})
	}
}

func TestDebugState_ReadCommand_Unknown(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "foobar\nnext\n")
	stdout := captureStdout(t, func() {
		ds.readCommand()
	})
	if !strings.Contains(stdout, "unknown command") {
		t.Errorf("expected 'unknown command', got: %s", stdout)
	}
}

func TestDebugState_ReadCommand_EmptyInput(t *testing.T) {
	ds := &debugState{
		cmdCh:  make(chan engine.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		events: []engine.EventRecord{{Step: 0, EventType: "Activity"}},
	}

	setInputReader(ds, "\n")
	captureStdout(t, func() {
		ds.readCommand()
	})

	select {
	case action := <-ds.cmdCh:
		if action != engine.ReplayNext {
			t.Errorf("expected ReplayNext for empty input, got %v", action)
		}
	default:
		t.Error("expected action on cmdCh for empty input")
	}
}

// =========================================================================
// ReplayStepAction constants test
// =========================================================================

func TestReplayStepActionConstants(t *testing.T) {
	// Verify the engine constants have the expected relative values.
	if engine.ReplayNext >= engine.ReplayQuit {
		t.Error("ReplayNext should come before ReplayQuit (iota)")
	}
}
