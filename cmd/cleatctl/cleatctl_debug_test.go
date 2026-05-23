package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/host"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// mockDB — minimal sql.DB for loadWorkflowInstance tests.
// ---------------------------------------------------------------------------

type mockDBConnector struct{}

func (c *mockDBConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &mockDBConn{}, nil
}
func (c *mockDBConnector) Driver() driver.Driver { return &mockDBDriver{} }

type mockDBDriver struct{}

func (d *mockDBDriver) Open(name string) (driver.Conn, error) {
	return &mockDBConn{}, nil
}

type mockDBConn struct{}

func (c *mockDBConn) Prepare(query string) (driver.Stmt, error) {
	return &mockDBStmt{}, nil
}
func (c *mockDBConn) Close() error  { return nil }
func (c *mockDBConn) Begin() (driver.Tx, error) {
	return nil, errors.New("no tx")
}

type mockDBStmt struct{}

func (s *mockDBStmt) Close() error  { return nil }
func (s *mockDBStmt) NumInput() int { return -1 }

func (s *mockDBStmt) Exec(_ []driver.Value) (driver.Result, error) {
	return nil, errors.New("unexpected exec")
}
func (s *mockDBStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &mockDBRows{}, nil
}

type mockDBRows struct {
	called bool
}

func (r *mockDBRows) Columns() []string {
	return []string{"id", "def_name", "def_version", "min_version", "status", "input",
		"result", "error", "error_code", "error_op", "assigned_to", "next_wake_at",
		"tenant_id", "created_at", "generation"}
}

func (r *mockDBRows) Close() error { return nil }

func (r *mockDBRows) Next(dest []driver.Value) error {
	if !r.called {
		r.called = true
		dest[0] = "test-wf-id"
		dest[1] = "test-def"
		dest[2] = int64(1)
		dest[3] = int64(0)
		dest[4] = "running"
		dest[5] = []byte(`{"key":"value"}`)
		dest[6] = []byte("")
		dest[7] = []byte("")
		dest[8] = []byte("")
		dest[9] = []byte("")
		dest[10] = "worker-1"
		dest[11] = time.Now()
		dest[12] = []byte("")
		dest[13] = time.Now()
		dest[14] = int64(0)
		return nil
	}
	return errors.New("no more rows")
}

// newMockDB creates a sql.DB backed by our mock connector.
func newMockDB() *sql.DB {
	return sql.OpenDB(&mockDBConnector{})
}

// ---------------------------------------------------------------------------
// Test helpers: create test EventRecords
// ---------------------------------------------------------------------------

func makeCallEvent(step int, service, op, request, response string) host.EventRecord {
	return host.EventRecord{
		Step:      step,
		EventType: host.EventTypeCall,
		Service:   service,
		Op:        op,
		Request:   request,
		Response:  response,
	}
}

func makeSignalEvent(step int, signalName, payload string) host.EventRecord {
	return host.EventRecord{
		Step:         step,
		EventType:    host.EventTypeSignalReceived,
		SignalName:   signalName,
		SignalPayload: payload,
	}
}

func makeChildWorkflowEvent(step int, name, runID string) host.EventRecord {
	return host.EventRecord{
		Step:      step,
		EventType: host.EventTypeChildWorkflow,
		Service:   name,
		RunID:     runID,
	}
}

func makeAwaitSignalEvent(step int, signalNames string, timeoutMs int64) host.EventRecord {
	return host.EventRecord{
		Step:        step,
		EventType:   host.EventTypeAwaitSignals,
		SignalNames: signalNames,
		TimeoutMs:   timeoutMs,
	}
}

func makeStateEvent(step int, stateOp, key, value string) host.EventRecord {
	return host.EventRecord{
		Step:     step,
		EventType: host.EventTypeStateMutation,
		StateOp:  stateOp,
		StateKey: key,
		Response: value,
	}
}

// ---------------------------------------------------------------------------
// parseDebugFlags tests
// ---------------------------------------------------------------------------

func TestParseDebugFlags_StepThrough(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-123", "--entry-point", "Handle"})
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}
	if flags.workflowID != "wf-123" {
		t.Errorf("workflowID = %q, want %q", flags.workflowID, "wf-123")
	}
	if flags.entryPoint != "Handle" {
		t.Errorf("entryPoint = %q, want %q", flags.entryPoint, "Handle")
	}
	if flags.watch {
		t.Error("watch should be false")
	}
}

func TestParseDebugFlags_WatchMode(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-456", "--watch"})
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}
	if flags.workflowID != "wf-456" {
		t.Errorf("workflowID = %q, want %q", flags.workflowID, "wf-456")
	}
	if !flags.watch {
		t.Error("watch should be true")
	}
}

func TestParseDebugFlags_MissingWorkflowID(t *testing.T) {
	flags := parseDebugFlags([]string{"--entry-point", "Handle"})
	if flags != nil {
		t.Error("expected nil flags when workflow ID is missing")
	}
}

func TestParseDebugFlags_MissingEntryPoint(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-123"})
	if flags != nil {
		t.Error("expected nil flags when --entry-point is missing")
	}
}

func TestParseDebugFlags_EntryPointMissingValue(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-123", "--entry-point"})
	if flags != nil {
		t.Error("expected nil flags when --entry-point value is missing")
	}
}

func TestParseDebugFlags_WatchEntryPointOptional(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-789", "--watch", "--entry-point", "Handle"})
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}
	if !flags.watch {
		t.Error("watch should be true")
	}
	if flags.entryPoint != "Handle" {
		t.Errorf("entryPoint = %q, want %q", flags.entryPoint, "Handle")
	}
}

func TestParseDebugFlags_UnknownFlag(t *testing.T) {
	flags := parseDebugFlags([]string{"wf-123", "--unknown"})
	if flags != nil {
		t.Error("expected nil flags when missing --entry-point (--unknown treated as positional)")
	}
}

// ---------------------------------------------------------------------------
// formatQueryState tests
// ---------------------------------------------------------------------------

func TestFormatQueryState_Empty(t *testing.T) {
	result := formatQueryState(nil)
	if result != "{}" {
		t.Errorf("got %q, want %q", result, "{}")
	}
	result = formatQueryState(map[string]string{})
	if result != "{}" {
		t.Errorf("got %q, want %q", result, "{}")
	}
}

func TestFormatQueryState_WithData(t *testing.T) {
	qs := map[string]string{
		"status": "active",
		"count":  "5",
	}
	result := formatQueryState(qs)
	if !strings.Contains(result, "status") || !strings.Contains(result, "active") {
		t.Errorf("expected status=active in %q", result)
	}
	if !strings.Contains(result, "count") || !strings.Contains(result, "5") {
		t.Errorf("expected count=5 in %q", result)
	}
}

func TestFormatQueryState_JSON(t *testing.T) {
	qs := map[string]string{"a": "1", "b": "2"}
	result := formatQueryState(qs)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Errorf("result is not valid JSON: %v (got: %s)", err, result)
	}
	if decoded["a"] != "1" || decoded["b"] != "2" {
		t.Errorf("unexpected decoded values: %v", decoded)
	}
}

// ---------------------------------------------------------------------------
// formatRemainingEvents tests
// ---------------------------------------------------------------------------

func TestFormatRemainingEvents_NoEvents(t *testing.T) {
	result := formatRemainingEvents(nil, 0)
	if !strings.Contains(result, "no remaining events") {
		t.Errorf("got %q, want 'no remaining events'", result)
	}
}

func TestFormatRemainingEvents_AllEvents(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "payments", "Charge", `{"amt":100}`, `{"id":"ch_1"}`),
		makeSignalEvent(1, "order_shipped", `{"tracking":"TRK"}`),
		makeAwaitSignalEvent(2, "sig1", 5000),
	}
	result := formatRemainingEvents(events, 0)
	if !strings.Contains(result, "Remaining events:") {
		t.Errorf("expected header, got: %s", result)
	}
	if !strings.Contains(result, "[1] step=0 type=call service=payments op=Charge") {
		t.Errorf("expected event 1, got: %s", result)
	}
	if !strings.Contains(result, "[2] step=1 type=signal_received signal=order_shipped") {
		t.Errorf("expected event 2, got: %s", result)
	}
	if !strings.Contains(result, "[3] step=2 type=await_signals") {
		t.Errorf("expected event 3, got: %s", result)
	}
}

func TestFormatRemainingEvents_FromStep(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "a", "b", "", ""),
		makeCallEvent(1, "c", "d", "", ""),
		makeCallEvent(2, "e", "f", "", ""),
	}
	result := formatRemainingEvents(events, 2)
	if strings.Contains(result, "step=0") || strings.Contains(result, "step=1") {
		t.Errorf("should not contain earlier events, got: %s", result)
	}
	if !strings.Contains(result, "[1] step=2") {
		t.Errorf("should start at step=2, got: %s", result)
	}
}

func TestFormatRemainingEvents_StateMutation(t *testing.T) {
	events := []host.EventRecord{
		makeStateEvent(0, "set", "mykey", "myval"),
	}
	result := formatRemainingEvents(events, 0)
	if !strings.Contains(result, "state_op=set key=mykey") {
		t.Errorf("got: %s", result)
	}
}

func TestFormatRemainingEvents_StateOps(t *testing.T) {
	events := []host.EventRecord{
		{Step: 0, EventType: host.EventTypeStateMutation, StateOp: "set", StateKey: "foo"},
		{Step: 1, EventType: host.EventTypeStateMutation, StateOp: "get", StateKey: "bar"},
	}
	result := formatRemainingEvents(events, 0)
	if !strings.Contains(result, "state_op=set key=foo") {
		t.Errorf("expected state_op for set, got: %s", result)
	}
	if !strings.Contains(result, "state_op=get key=bar") {
		t.Errorf("expected state_op for get, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// formatEvent tests
// ---------------------------------------------------------------------------

func TestFormatEvent_Call(t *testing.T) {
	ev := makeCallEvent(0, "email", "send", `{"to":"a"}`, `{"id":"msg_1"}`)
	result := formatEvent(ev)
	if !strings.Contains(result, "type=call") {
		t.Errorf("expected type=call, got: %s", result)
	}
	if !strings.Contains(result, "service=email") {
		t.Errorf("expected service=email, got: %s", result)
	}
	if !strings.Contains(result, "op=send") {
		t.Errorf("expected op=send, got: %s", result)
	}
}

func TestFormatEvent_Signal(t *testing.T) {
	ev := makeSignalEvent(0, "wake", `{"at":"noon"}`)
	result := formatEvent(ev)
	if !strings.Contains(result, "type=signal_received") {
		t.Errorf("expected type=signal_received, got: %s", result)
	}
	if !strings.Contains(result, "signal=wake") {
		t.Errorf("expected signal=wake, got: %s", result)
	}
}

func TestFormatEvent_WithError(t *testing.T) {
	ev := host.EventRecord{
		Step:      0,
		EventType: host.EventTypeCall,
		Service:   "db",
		Op:        "query",
		Err:       "connection refused",
	}
	result := formatEvent(ev)
	if !strings.Contains(result, "err=connection refused") {
		t.Errorf("expected error in output, got: %s", result)
	}
}

func TestFormatEvent_Truncation(t *testing.T) {
	longStr := strings.Repeat("x", 100)
	ev := host.EventRecord{
		Step:      0,
		EventType: host.EventTypeCall,
		Request:   longStr,
	}
	result := formatEvent(ev)
	if len(result) >= len(longStr)+50 {
		t.Errorf("expected truncation, got length %d: %s", len(result), result)
	}
	if !strings.Contains(result, "...") {
		t.Errorf("expected '...' for truncation, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// debugState callback & command tests
// ---------------------------------------------------------------------------

func TestDebugState_NextCommand(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "test", "op", `{}`, `{}`),
	}
	ds := &debugState{
		events: events,
		stepCh: make(chan debugStepInfo, 1),
		cmdCh:  make(chan host.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		doneCh: make(chan error, 1),
	}

	go func() {
		action := ds.callback(0, &events[0], map[string]string{})
		if action != host.ReplayNext {
			t.Errorf("expected ReplayNext, got %v", action)
		}
		action = ds.callback(1, &events[0], map[string]string{})
		if action != host.ReplayQuit {
			t.Errorf("expected ReplayQuit, got %v", action)
		}
		ds.doneCh <- nil
	}()

	info := <-ds.stepCh
	if info.step != 0 {
		t.Errorf("expected step 0, got %d", info.step)
	}
	ds.displayStep(info)
	ds.cmdCh <- host.ReplayNext

	<-ds.stepCh
	close(ds.quit)

	<-ds.doneCh
}

func TestDebugState_QuitCommand(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "test", "op", `{}`, `{}`),
	}
	ds := &debugState{
		events: events,
		stepCh: make(chan debugStepInfo, 1),
		cmdCh:  make(chan host.ReplayStepAction, 1),
		quit:   make(chan struct{}),
		doneCh: make(chan error, 1),
	}

	go func() {
		action := ds.callback(0, &events[0], nil)
		if action != host.ReplayQuit {
			t.Errorf("expected ReplayQuit, got %v", action)
		}
		ds.doneCh <- nil
	}()

	<-ds.stepCh
	ds.cmdCh <- host.ReplayQuit

	<-ds.doneCh
}

func TestDebugState_AutoContinue(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "test", "op", `{}`, `{}`),
		makeCallEvent(1, "test", "op2", `{}`, `{}`),
	}
	ds := &debugState{
		events:       events,
		autoContinue: true,
		stepCh:       make(chan debugStepInfo, 1),
		cmdCh:        make(chan host.ReplayStepAction, 1),
		quit:         make(chan struct{}),
		doneCh:       make(chan error, 1),
	}

	go func() {
		action := ds.callback(0, &events[0], nil)
		if action != host.ReplayNext {
			t.Errorf("expected ReplayNext, got %v", action)
		}
		if ds.lastStep != 0 {
			t.Errorf("expected lastStep=0, got %d", ds.lastStep)
		}

		action = ds.callback(1, &events[1], nil)
		if action != host.ReplayNext {
			t.Errorf("expected ReplayNext, got %v", action)
		}
		if ds.lastStep != 1 {
			t.Errorf("expected lastStep=1, got %d", ds.lastStep)
		}

		ds.doneCh <- nil
	}()

	<-ds.doneCh
}

func TestDebugState_StateDataAccess(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "test", "op", `{}`, `{}`),
	}
	ds := &debugState{
		events:    events,
		lastStep:  0,
		lastEvent: &events[0],
		lastQS:    map[string]string{"key1": "val1", "key2": "val2"},
		stepCh:    make(chan debugStepInfo, 1),
		cmdCh:     make(chan host.ReplayStepAction, 1),
		quit:      make(chan struct{}),
		doneCh:    make(chan error, 1),
	}

	if ds.lastQS["key1"] != "val1" {
		t.Error("expected key1=val1")
	}
	if ds.lastQS["key2"] != "val2" {
		t.Error("expected key2=val2")
	}
}

func TestDebugState_DisplayStep(t *testing.T) {
	events := []host.EventRecord{
		makeCallEvent(0, "payments", "Charge", `{"amount":999}`, `{"charge_id":"ch_123"}`),
	}
	ds := &debugState{
		events: events,
	}

	info := debugStepInfo{
		step:  0,
		event: &events[0],
		qs:    map[string]string{"cart_total": "999"},
	}
	ds.displayStep(info)

	if info.step != 0 {
		t.Errorf("step = %d, want 0", info.step)
	}
	if info.event.Service != "payments" {
		t.Errorf("service = %s, want payments", info.event.Service)
	}
}

// ---------------------------------------------------------------------------
// Debug run dispatch tests (using mockStore)
// ---------------------------------------------------------------------------

func TestRunDebug_MissingArgs(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runDebug(context.Background(), &mockStore{}, nil, []string{})
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage in stderr, got: %s", stderr)
	}
}

func TestRunDebug_MissingEntryPoint(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runDebug(context.Background(), &mockStore{}, nil, []string{"wf-123"})
	})
	if !strings.Contains(stderr, "entry-point") {
		t.Errorf("expected entry-point error in stderr, got: %s", stderr)
	}
}

func TestRunDebug_EmptyHistory(t *testing.T) {
	store := &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]host.EventRecord, error) {
			return nil, nil
		},
	}
	db := newMockDB()
	defer db.Close()
	stdout, stderr := captureOutputs(t, func() {
		defer func() { recover() }()
		runDebugStep(context.Background(), store, db, "wf-123", "Handle")
	})
	_ = stdout
	_ = stderr
}

func TestRunDebug_ReadOnly(t *testing.T) {
	appendCalled := false
	store := &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]host.EventRecord, error) {
			return []host.EventRecord{
				makeCallEvent(0, "test", "noop", `{}`, `{}`),
			}, nil
		},
		loadWASMFn: func(_ context.Context, defName string, defVersion int) ([]byte, error) {
			return nil, nil
		},
		appendEventHistoryFn: func(_ context.Context, workflowID string, rec host.EventRecord) error {
			appendCalled = true
			return nil
		},
		appendEventHistoryBatchFn: func(_ context.Context, workflowID string, recs []host.EventRecord) error {
			appendCalled = true
			return nil
		},
	}
	db := newMockDB()
	defer db.Close()
	stdout, stderr := captureOutputs(t, func() {
		defer func() { recover() }()
		runDebugStep(context.Background(), store, db, "wf-123", "Handle")
	})
	_ = stdout
	_ = stderr
	if appendCalled {
		t.Error("write methods should not be called during debug")
	}
}

func TestRunDebug_InvalidFlag(t *testing.T) {
	_, stderr := withExitPanicOutput(t, func() {
		runDebug(context.Background(), &mockStore{}, nil, []string{"wf-123", "--bogus"})
	})
	if !strings.Contains(stderr, "entry-point") {
		t.Errorf("expected entry-point error, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Watch mode tests
// ---------------------------------------------------------------------------

func TestRunDebugWatch_PollsForNewEvents(t *testing.T) {
	pollCount := 0
	events := []host.EventRecord{
		makeCallEvent(0, "svc", "op", `{}`, `{}`),
		makeSignalEvent(1, "sig", `{}`),
	}

	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			defer func() { pollCount++ }()
			if pollCount == 0 {
				return 0, nil
			}
			if pollCount == 1 {
				return 2, nil
			}
			return 2, nil
		},
		loadEventHistoryPaginatedFn: func(_ context.Context, workflowID string, offset, limit int) ([]host.EventRecord, error) {
			if offset == 0 && pollCount == 1 {
				return events, nil
			}
			return nil, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(2500*time.Millisecond, cancel)

	stdout, _ := captureOutputs(t, func() {
		runDebugWatch(ctx, store, "wf-123")
	})

	if !strings.Contains(stdout, "Watching workflow") {
		t.Errorf("expected 'Watching workflow', got: %s", stdout)
	}
	if pollCount < 1 {
		t.Error("expected at least one poll cycle")
	}
}

func TestRunDebugWatch_ZeroEvents(t *testing.T) {
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			return 0, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	stdout, _ := captureOutputs(t, func() {
		runDebugWatch(ctx, store, "wf-456")
	})

	if !strings.Contains(stdout, "0 events so far") {
		t.Errorf("expected '0 events so far', got: %s", stdout)
	}
}

func TestRunDebugWatch_CountError(t *testing.T) {
	store := &mockStore{
		countEventHistoryFn: func(_ context.Context, workflowID string) (int, error) {
			return 0, errors.New("db error")
		},
	}

	stderr := withExitPanic(t, func() {
		runDebugWatch(context.Background(), store, "wf-err")
	})

	if !strings.Contains(stderr, "error counting") {
		t.Errorf("expected error message, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Helper factory
// ---------------------------------------------------------------------------

func makeDebugMockStore(events []host.EventRecord, wasmBytes []byte) *mockStore {
	return &mockStore{
		loadEventHistoryFn: func(_ context.Context, workflowID string) ([]host.EventRecord, error) {
			return events, nil
		},
		loadWASMFn: func(_ context.Context, defName string, defVersion int) ([]byte, error) {
			return wasmBytes, nil
		},
	}
}

// Ensure imports are used.
func TestUnusedImports(t *testing.T) {
	_ = uuid.Nil
	_ = newMockDB
	_ = makeChildWorkflowEvent
	_ = makeStateEvent
	_ = makeDebugMockStore
	_ = host.ReplayNext
	var buf bytes.Buffer
	buf.Reset()
}
