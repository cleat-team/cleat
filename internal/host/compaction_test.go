package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestPluginCallCompactionRoundTrip verifies that plugin_call and
// plugin_call_stream_chunk events survive a compaction round-trip.
func TestPluginCallCompactionRoundTrip(t *testing.T) {
	events := []EventRecord{
		{
			Step:      0,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"result":"ok"}`,
			PluginError:  "",
		},
		{
			Step:      1,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":1}`,
			PluginError:  "",
		},
		{
			Step:      2,
			EventType: EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "DoSomething",
			PluginInput:  `{"key":"value"}`,
			PluginOutput: `{"chunk":2,"finish":true}`,
			PluginError:  "",
		},
		{
			Step:      3,
			EventType: EventTypePluginCall,
			PluginName:   "test-plugin",
			PluginFunc:   "GetObject",
			PluginInput:  `{"bucket":"x","key":"y"}`,
			PluginOutput: "",
			PluginError:  "not found",
		},
	}

	// Compact at every possible split point and verify round-trip.
	for split := 1; split <= len(events); split++ {
		compacted := events[:split]
		tail := events[split:]

		cs := extractCompactionState(compacted)
		reconstructed := buildFullHistoryFromCompaction(tail, cs)

		if len(reconstructed) != len(events) {
			t.Errorf("split=%d: length mismatch: got %d events, want %d",
				split, len(reconstructed), len(events))
			continue
		}

		for i := range events {
			if !eventFieldsMatch(events[i], reconstructed[i]) {
				t.Errorf("split=%d: event %d (%s) mismatch", split, i, events[i].EventType)
				dumpEventDiff(t, events[i], reconstructed[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Compaction unit tests (pure functions, no DB needed)
// ---------------------------------------------------------------------------

// TestCompactionBelowThreshold verifies that extractCompactionState handles a
// small number of events correctly and buildFullHistoryFromCompaction
// reconstructs them faithfully. The threshold logic is in
// CompactWorkflowHistory (store-level), so here we test the pure functions.
func TestCompactionBelowThreshold(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2", Request: `{}`, Response: `{"ok":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op3", Request: `{}`, Response: `{"ok":true}`},
	}

	cs := extractCompactionState(events)
	if cs == nil {
		t.Fatal("expected non-nil CompactionState")
	}
	if cs.CompactedStep != 3 {
		t.Errorf("expected CompactedStep=3, got %d", cs.CompactedStep)
	}
	if len(cs.Events) != 3 {
		t.Fatalf("expected 3 compacted events, got %d", len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != 3 {
		t.Fatalf("expected 3 reconstructed events, got %d", len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d mismatch", i)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionAboveThreshold verifies that a large set of events is
// correctly compacted and reconstructed with the same semantics used by
// CompactWorkflowHistory (keep the most recent threshold/2 as tail).
func TestCompactionAboveThreshold(t *testing.T) {
	threshold := 10
	nEvents := 25
	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op%d", i),
			Request:   `{}`,
			Response:  fmt.Sprintf(`{"step":%d}`, i),
		}
	}

	// Simulate logic from CompactWorkflowHistory:
	// If len > threshold, keepStep = len - threshold/2
	keepStep := nEvents - threshold/2
	if keepStep < 0 {
		keepStep = 0
	}
	compacted := events[:keepStep]
	tail := events[keepStep:]

	cs := extractCompactionState(compacted)
	if len(cs.Events) != keepStep {
		t.Fatalf("expected %d compacted events, got %d", keepStep, len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(tail, cs)
	if len(reconstructed) != nEvents {
		t.Fatalf("expected %d events, got %d", nEvents, len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d mismatch", i)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionPreservesRecentEvents verifies that the tail (most recent
// events) survives compaction untouched — they are passed through directly
// without being transformed into CompactedEvent and back.
func TestCompactionPreservesRecentEvents(t *testing.T) {
	nEvents := 15
	tailSize := 5

	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op%d", i),
			Request:   fmt.Sprintf(`{"req":%d}`, i),
			Response:  fmt.Sprintf(`{"step":%d}`, i),
		}
	}

	compacted := events[:nEvents-tailSize]
	tail := events[nEvents-tailSize:]

	cs := extractCompactionState(compacted)
	reconstructed := buildFullHistoryFromCompaction(tail, cs)

	// The tail events should be identical objects (not reconstructed from JSON).
	for i, te := range tail {
		if reconstructed[len(compacted)+i].Response != te.Response {
			t.Errorf("tail event %d response: expected %q, got %q",
				i, te.Response, reconstructed[len(compacted)+i].Response)
		}
	}
}

// TestCompactionOfCompletedWorkflow verifies that all events of a completed
// workflow can be compacted (no tail) and the reconstructed history is
// faithful.
func TestCompactionOfCompletedWorkflow(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"id":"1"}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "process", Request: `{}`, Response: `{"done":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "finish", Request: `{}`, Response: `{"ok":true}`},
		{Step: 3, EventType: EventTypeSleep, DurationMs: 5000},
		{Step: 4, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
		{Step: 5, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
	}

	// Full compaction: all events are compacted, tail is nil.
	cs := extractCompactionState(events)
	if cs.CompactedStep != len(events) {
		t.Errorf("expected CompactedStep=%d, got %d", len(events), cs.CompactedStep)
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(reconstructed))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d (%s) mismatch", i, events[i].EventType)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// TestCompactionOfRunningWorkflow verifies that only old events are compacted
// while recent events (the tail) remain as full EventRecords. This simulates
// a running workflow where the DB still has recent events as rows while old
// ones have been collapsed into CompactionState.
func TestCompactionOfRunningWorkflow(t *testing.T) {
	// Simulate a running workflow with steps: call*5, sleep, await_signals
	oldEvents := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"id":"1"}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "prepare", Request: `{}`, Response: `{"done":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "process", Request: `{}`, Response: `{"ok":true}`},
		{Step: 3, EventType: EventTypeSleep, DurationMs: 30000},
	}
	recentEvents := []EventRecord{
		{Step: 4, EventType: EventTypeAwaitSignals, SignalNames: "payment,approval", TimeoutMs: 60000},
		{Step: 5, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
	}

	cs := extractCompactionState(oldEvents)
	reconstructed := buildFullHistoryFromCompaction(recentEvents, cs)

	expectedLen := len(oldEvents) + len(recentEvents)
	if len(reconstructed) != expectedLen {
		t.Fatalf("expected %d events, got %d", expectedLen, len(reconstructed))
	}

	// Verify old events match.
	for i := range oldEvents {
		if !eventFieldsMatch(oldEvents[i], reconstructed[i]) {
			t.Errorf("old event %d mismatch", i)
			dumpEventDiff(t, oldEvents[i], reconstructed[i])
		}
	}

	// Verify recent events are preserved (passed through directly).
	for i := range recentEvents {
		idx := len(oldEvents) + i
		if reconstructed[idx].SignalName != recentEvents[i].SignalName {
			t.Errorf("recent event %d signal name: expected %q, got %q",
				i, recentEvents[i].SignalName, reconstructed[idx].SignalName)
		}
	}
}

// TestCompactionRoundTripThenReplay verifies the full round-trip: extract
// compaction state from events, then reconstruct the full history, then
// verify that the reconstruction matches the original exactly for all
// supported event types.
func TestCompactionRoundTripThenReplay(t *testing.T) {
	// Include all supported event types to exercise every branch of
	// extractCompactionState and buildFullHistoryFromCompaction.
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "do", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "fail", Request: `{}`, Response: ``, Err: "timeout"},
		{Step: 2, EventType: EventTypeSleep, DurationMs: 5000},
		{Step: 3, EventType: EventTypeAwaitSignals, SignalNames: "sig1,sig2", TimeoutMs: 10000},
		{Step: 4, EventType: EventTypeSignalReceived, SignalName: "sig1", SignalPayload: `{"data":"hello"}`},
		{Step: 5, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup DB"},
		{Step: 6, EventType: EventTypeChildWorkflow, ChildName: "child-wf", ChildInput: `{"x":1}`, RunID: "run-001"},
		{Step: 7, EventType: EventTypeAwaitChild, RunID: "run-001", Response: `{"result":"done"}`},
		{Step: 8, EventType: EventTypeAwaitChild, RunID: "run-002", Err: "child failed"},
		{Step: 9, EventType: EventTypeContinueAsNew, NewInput: `{"restart":true}`},
		{Step: 10, EventType: EventTypeHeartbeat, Service: "svc", Op: "long-op"},
		{Step: 11, EventType: EventTypeAwaitAllChildren, Response: `[{"run_id":"c1","result":"ok"}]`},
		{Step: 12, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f",
			PluginInput: `{"x":1}`, PluginOutput: `{"result":"ok"}`, PluginError: ""},
		{Step: 13, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "g",
			PluginInput: `{}`, PluginOutput: ``, PluginError: "not found"},
		{Step: 14, EventType: EventTypeCreatePromise, PromiseName: "prom-1", PromiseID: "pid-001"},
		{Step: 15, EventType: EventTypeAwaitPromise, PromiseID: "pid-001"},
		{Step: 16, EventType: EventTypePromiseResolved, PromiseID: "pid-001", PromiseResult: `{"status":"ok"}`},
		{Step: 17, EventType: EventTypePromiseRejected, PromiseID: "pid-002", PromiseError: "card declined"},
		{Step: 18, EventType: EventTypeUpdateHandler, UpdateHandlerName: "update-shipping"},
		{Step: 19, EventType: EventTypeStateMutation, StateKey: "count", StateValue: "3", StateDelta: 1, StateOp: "increment"},
		{Step: 20, EventType: EventTypeRunDetached},
		{Step: 21, EventType: EventTypeAcquireLock, LockKey: "my-lock", LockTTLMs: 30000, LockAcquired: 1},
		{Step: 22, EventType: EventTypeReleaseLock, LockKey: "my-lock"},
		{Step: 23, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:123:"},
		{Step: 24, EventType: EventTypePluginCallStreamChunk,
			PluginName: "p", PluginFunc: "f", PluginOutput: `{"chunk":1}`, StreamChunkIndex: 0, StreamFinish: true},
	}

	// Compact all events (simulating a fully compacted workflow) and reconstruct.
	cs := extractCompactionState(events)
	if len(cs.Events) != len(events) {
		t.Fatalf("expected %d compacted events, got %d", len(events), len(cs.Events))
	}

	reconstructed := buildFullHistoryFromCompaction(nil, cs)
	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d reconstructed events, got %d", len(events), len(reconstructed))
	}

	for i := range events {
		if !eventFieldsMatch(events[i], reconstructed[i]) {
			t.Errorf("event %d (%s) round-trip mismatch", i, events[i].EventType)
			dumpEventDiff(t, events[i], reconstructed[i])
		}
	}
}

// ---------------------------------------------------------------------------
// CompactWorkflowHistory tests (store-level)
// ---------------------------------------------------------------------------

// mockCompactStore implements WorkflowStore for testing CompactWorkflowHistory.
type mockCompactStore struct {
	events         []EventRecord
	loadErr        error
	compactErr     error
	compactWorkflowID string
	compactState      []byte
	compactStep       int
	keepStep          int
	loadCount         int
	compactCount      int
}

func (m *mockCompactStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	m.loadCount++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return m.events, nil
}

func (m *mockCompactStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	m.compactCount++
	m.compactWorkflowID = workflowID
	m.compactState = compactionState
	m.compactStep = compactionStep
	m.keepStep = keepStep
	return m.compactErr
}

// satisfy the rest of the WorkflowStore interface with no-ops.
func (m *mockCompactStore) ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCompactStore) ClaimWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCompactStore) ClaimStickyWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) { return nil, nil }
func (m *mockCompactStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error { return nil }
func (m *mockCompactStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error { return nil }
func (m *mockCompactStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) { return nil, nil }
func (m *mockCompactStore) ListVersions(ctx context.Context, defName string) ([]int, error) { return nil, nil }
func (m *mockCompactStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) { return false, nil }
func (m *mockCompactStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error { return nil }
func (m *mockCompactStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error { return nil }
func (m *mockCompactStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error { return nil }
func (m *mockCompactStore) RequestCancellation(ctx context.Context, workflowID, reason string) error { return nil }
func (m *mockCompactStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) { return false, "", nil }
func (m *mockCompactStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error { return nil }
func (m *mockCompactStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) { return "", false, nil }
func (m *mockCompactStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) { return "", false, nil }
func (m *mockCompactStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error) { return "", nil }
func (m *mockCompactStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) { return "", false, nil }
func (m *mockCompactStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) { return 0, nil }
func (m *mockCompactStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) { return "", nil }
func (m *mockCompactStore) ListWorkflows(ctx context.Context, status string, limit int) ([]WorkflowInstance, error) { return nil, nil }
func (m *mockCompactStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) { return nil, nil }
func (m *mockCompactStore) CreateSchedule(ctx context.Context, s Schedule) error { return nil }
func (m *mockCompactStore) ListSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCompactStore) DeleteSchedule(ctx context.Context, name string) error { return nil }
func (m *mockCompactStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error { return nil }
func (m *mockCompactStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) { return nil, nil }
func (m *mockCompactStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error { return nil }
func (m *mockCompactStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) { return 0, nil }
func (m *mockCompactStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) { return nil, nil }
func (m *mockCompactStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error) { return nil, nil }
func (m *mockCompactStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) { return nil, nil }
func (m *mockCompactStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) { return nil, nil }
func (m *mockCompactStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error { return nil }
func (m *mockCompactStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error { return nil }
func (m *mockCompactStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error { return nil }
func (m *mockCompactStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) { return "", "", "", nil }
func (m *mockCompactStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) { return nil, nil }
func (m *mockCompactStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error { return nil }
func (m *mockCompactStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) { return nil, nil }
func (m *mockCompactStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error { return nil }
func (m *mockCompactStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) { return false, nil }
func (m *mockCompactStore) ReleaseConcurrencyKey(ctx context.Context, key string) error { return nil }
func (m *mockCompactStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error { return nil }
func (m *mockCompactStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) { return 0, nil }
func (m *mockCompactStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error { return nil }
func (m *mockCompactStore) ClearStickyWorker(ctx context.Context, workflowID string) error { return nil }
func (m *mockCompactStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error { return nil }
func (m *mockCompactStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) { return nil, nil }
func (m *mockCompactStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) { return nil, nil }
func (m *mockCompactStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error { return nil }
func (m *mockCompactStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error { return nil }
func (m *mockCompactStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) { return 0, nil }
func (m *mockCompactStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) { return nil, nil }

func TestCompactWorkflowHistory_EmptyHistory(t *testing.T) {
	// Empty event history should not trigger compaction.
	store := &mockCompactStore{events: []EventRecord{}}
	err := CompactWorkflowHistory(context.Background(), store, "wf-001", DefaultCompactionThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.loadCount != 1 {
		t.Errorf("expected 1 LoadEventHistory call, got %d", store.loadCount)
	}
	if store.compactCount != 0 {
		t.Errorf("expected 0 CompactHistory calls for empty history, got %d", store.compactCount)
	}
}

func TestCompactWorkflowHistory_SingleEvent(t *testing.T) {
	// Single event below threshold — should not compact.
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":true}`},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-002", DefaultCompactionThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 0 {
		t.Errorf("expected 0 CompactHistory calls for single event, got %d", store.compactCount)
	}
}

func TestCompactWorkflowHistory_BelowThreshold(t *testing.T) {
	// DefaultCompactionThreshold=1000 events — below threshold should not compact.
	events := make([]EventRecord, 500)
	for i := 0; i < 500; i++ {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-003", DefaultCompactionThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 0 {
		t.Errorf("expected 0 CompactHistory calls for 500 events (below %d), got %d", DefaultCompactionThreshold, store.compactCount)
	}
}

func TestCompactWorkflowHistory_AboveThreshold(t *testing.T) {
	// 1500 events with threshold=1000 → should compact (1500 > 1000).
	// keepStep = len - threshold/2 = 1500 - 500 = 1000.
	// compactedStep = keepStep = 1000.
	threshold := 1000
	nEvents := 1500
	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}

	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-004", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.loadCount != 1 {
		t.Errorf("expected 1 LoadEventHistory call, got %d", store.loadCount)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 CompactHistory call, got %d", store.compactCount)
	}

	// Verify compaction parameters.
	expectedKeepStep := nEvents - threshold/2 // 1500 - 500 = 1000
	if store.keepStep != expectedKeepStep {
		t.Errorf("expected keepStep=%d, got %d", expectedKeepStep, store.keepStep)
	}
	if store.compactStep != expectedKeepStep {
		t.Errorf("expected compactStep=%d, got %d", expectedKeepStep, store.compactStep)
	}
	if store.compactWorkflowID != "wf-004" {
		t.Errorf("expected workflowID wf-004, got %q", store.compactWorkflowID)
	}
	if store.compactState == nil {
		t.Fatal("expected non-nil compaction state")
	}
}

func TestCompactWorkflowHistory_LoadError(t *testing.T) {
	// LoadEventHistory returns error → should propagate.
	store := &mockCompactStore{loadErr: fmt.Errorf("db error")}
	err := CompactWorkflowHistory(context.Background(), store, "wf-005", DefaultCompactionThreshold)
	if err == nil {
		t.Fatal("expected error from LoadEventHistory, got nil")
	}
}

func TestCompactWorkflowHistory_CompactError(t *testing.T) {
	// CompactHistory returns error → should propagate.
	nEvents := 1500
	events := make([]EventRecord, nEvents)
	for i := 0; i < nEvents; i++ {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}
	store := &mockCompactStore{events: events, compactErr: fmt.Errorf("store error")}
	err := CompactWorkflowHistory(context.Background(), store, "wf-006", 1000)
	if err == nil {
		t.Fatal("expected error from CompactHistory, got nil")
	}
}

func TestCompactWorkflowHistory_ExactThreshold(t *testing.T) {
	// Exactly threshold events should NOT compact (len <= threshold).
	n := DefaultCompactionThreshold
	events := make([]EventRecord, n)
	for i := 0; i < n; i++ {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-007", DefaultCompactionThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 0 {
		t.Errorf("expected 0 compactions at threshold boundary, got %d", store.compactCount)
	}
}

func TestCompactWorkflowHistory_CompactAll(t *testing.T) {
	// Very small threshold so every event triggers compaction.
	// CompactWorkflowHistory keeps the most recent threshold/2 events.
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1"},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2"},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op3"},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-008", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 compaction, got %d", store.compactCount)
	}
	// keepStep = len - threshold/2 = 3 - 1 = 2
	expectedKeepStep := len(events) - threshold/2
	if store.keepStep != expectedKeepStep {
		t.Errorf("expected keepStep=%d, got %d", expectedKeepStep, store.keepStep)
	}
}

func TestCompactWorkflowHistory_MultipleEventTypes(t *testing.T) {
	// Mixed event types to exercise extractCompactionState in the compaction path.
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeSleep, DurationMs: 5000},
		{Step: 2, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
		{Step: 3, EventType: EventTypeAwaitSignals, SignalNames: "payment", TimeoutMs: 30000},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-009", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 compaction, got %d", store.compactCount)
	}
	// Verify that the compaction state is valid JSON.
	if len(store.compactState) == 0 {
		t.Fatal("expected non-empty compaction state")
	}
	// Parse and verify basic structure.
	var cs CompactionState
	if err := json.Unmarshal(store.compactState, &cs); err != nil {
		t.Fatalf("compaction state is not valid JSON: %v", err)
	}
	if cs.Version != 1 {
		t.Errorf("expected compaction state version 1, got %d", cs.Version)
	}
}

func TestCompactWorkflowHistory_ExactThresholdPlusOne(t *testing.T) {
	// threshold + 1 events → should compact (len > threshold).
	threshold := 10
	events := make([]EventRecord, threshold+1)
	for i := 0; i < threshold+1; i++ {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-010", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.compactCount != 1 {
		t.Errorf("expected 1 compaction for threshold+1 events, got %d", store.compactCount)
	}
	// keepStep = len - threshold/2 = 11 - 5 = 6
	expectedKeepStep := (threshold + 1) - threshold/2
	if store.keepStep != expectedKeepStep {
		t.Errorf("expected keepStep=%d, got %d", expectedKeepStep, store.keepStep)
	}
}

func TestCompactWorkflowHistory_VerifyCompactionStateContent(t *testing.T) {
	// Verify the compaction state JSON contains the expected fields.
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
		{Step: 1, EventType: EventTypeSleep, DurationMs: 1000},
		{Step: 2, EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup"},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-011", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cs CompactionState
	if err := json.Unmarshal(store.compactState, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Steps 0..1 are compacted (keepStep = 3 - 1 = 2), step 2 is tail.
	expectedCompactedStep := len(events) - threshold/2 // 3 - 1 = 2
	if cs.CompactedStep != expectedCompactedStep {
		t.Errorf("expected CompactedStep=%d, got %d", expectedCompactedStep, cs.CompactedStep)
	}
	if len(cs.Events) != expectedCompactedStep {
		t.Errorf("expected %d compacted events, got %d", expectedCompactedStep, len(cs.Events))
	}

	// Check first event fields survived compaction.
	if cs.Events[0].Type != EventCodeCall {
		t.Errorf("expected EventCodeCall (%d), got %d", EventCodeCall, cs.Events[0].Type)
	}

	// Check second event.
	if cs.Events[1].Type != EventCodeSleep {
		t.Errorf("expected EventCodeSleep (%d), got %d", EventCodeSleep, cs.Events[1].Type)
	}
	if cs.Events[1].DurationMs != 1000 {
		t.Errorf("expected DurationMs=1000, got %d", cs.Events[1].DurationMs)
	}
}

func TestCompactWorkflowHistory_VerifyTailPreserved(t *testing.T) {
	// Verify that tail events are NOT included in compaction state.
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "old1"},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "old2"},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "recent1"},
		{Step: 3, EventType: EventTypeCall, Service: "svc", Op: "recent2"},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-012", threshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// keepStep = 4 - 1 = 3, so steps 0,1,2 are compacted and step 3 is tail.
	var cs CompactionState
	if err := json.Unmarshal(store.compactState, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(cs.Events) != 3 {
		t.Errorf("expected 3 compacted events (steps 0,1,2), got %d", len(cs.Events))
	}
}

// ---------------------------------------------------------------------------
// Compaction state extraction edge cases
// ---------------------------------------------------------------------------

// TestExtractCompactionState_WithOpenChildren verifies that child workflows
// started but not completed are tracked as open children in compaction state.
func TestExtractCompactionState_WithOpenChildren(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-a", ChildInput: `{"x":1}`, RunID: "run-a"},
		{Step: 1, EventType: EventTypeChildWorkflow, ChildName: "child-b", ChildInput: `{"y":2}`, RunID: "run-b"},
	}

	cs := extractCompactionState(events)
	if len(cs.OpenChildren) != 2 {
		t.Fatalf("expected 2 open children, got %d", len(cs.OpenChildren))
	}

	foundA := false
	foundB := false
	for _, c := range cs.OpenChildren {
		if c.RunID == "run-a" && c.Name == "child-a" && c.Input == `{"x":1}` {
			foundA = true
		}
		if c.RunID == "run-b" && c.Name == "child-b" && c.Input == `{"y":2}` {
			foundB = true
		}
	}
	if !foundA {
		t.Error("child-a not found in open children")
	}
	if !foundB {
		t.Error("child-b not found in open children")
	}
}

// TestExtractCompactionState_OpenChildrenClosed verifies that children with
// matching await_child completion events are NOT included in open children.
func TestExtractCompactionState_OpenChildrenClosed(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-a", ChildInput: `{"x":1}`, RunID: "run-a"},
		{Step: 1, EventType: EventTypeChildWorkflow, ChildName: "child-b", ChildInput: `{"y":2}`, RunID: "run-b"},
		{Step: 2, EventType: EventTypeAwaitChild, RunID: "run-a", Response: `{"ok":true}`},
	}

	cs := extractCompactionState(events)
	if len(cs.OpenChildren) != 1 {
		t.Fatalf("expected 1 open child (child-b), got %d", len(cs.OpenChildren))
	}
	if cs.OpenChildren[0].RunID != "run-b" {
		t.Errorf("expected open child run-b, got %s", cs.OpenChildren[0].RunID)
	}
}

// TestExtractCompactionState_AwaitAllChildrenResets verifies that an
// await_all_children event resets all open children.
func TestExtractCompactionState_AwaitAllChildrenResets(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-a", ChildInput: `{}`, RunID: "run-a"},
		{Step: 1, EventType: EventTypeChildWorkflow, ChildName: "child-b", ChildInput: `{}`, RunID: "run-b"},
		{Step: 2, EventType: EventTypeAwaitAllChildren, Response: `[{"ok":true}]`},
	}

	cs := extractCompactionState(events)
	if len(cs.OpenChildren) != 0 {
		t.Errorf("expected 0 open children after await_all, got %d", len(cs.OpenChildren))
	}
}

// TestExtractCompactionState_WithPendingDefers verifies that defer events
// are tracked as pending defers in compaction state.
func TestExtractCompactionState_WithPendingDefers(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup resources"},
		{Step: 1, EventType: EventTypeDefer, DeferID: "d2", DeferDescription: "close connection"},
	}

	cs := extractCompactionState(events)
	if len(cs.PendingDefers) != 2 {
		t.Fatalf("expected 2 pending defers, got %d", len(cs.PendingDefers))
	}

	found := make(map[string]bool)
	for _, d := range cs.PendingDefers {
		if d.ID == "d1" && d.Description == "cleanup resources" {
			found["d1"] = true
		}
		if d.ID == "d2" && d.Description == "close connection" {
			found["d2"] = true
		}
	}
	if !found["d1"] {
		t.Error("defer d1 not found in pending defers")
	}
	if !found["d2"] {
		t.Error("defer d2 not found in pending defers")
	}
}

// TestExtractCompactionState_SideEffectRoundTrip verifies that side_effect
// and scope_acquired events round-trip through compaction correctly.
// SideEffect is not covered by TestCompactionRoundTripThenReplay.
func TestExtractCompactionState_SideEffectRoundTrip(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeSideEffect, SideEffectResult: `{"random":42}`},
		{Step: 1, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:123"},
	}

	cs := extractCompactionState(events)
	reconstructed := buildFullHistoryFromCompaction(nil, cs)

	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(reconstructed))
	}

	// SideEffect fields are not stored in compacted form; verify behavior.
	if reconstructed[0].EventType != EventTypeSideEffect {
		t.Errorf("expected SideEffect event type, got %s", reconstructed[0].EventType)
	}

	// ScopeAcquired: ScopeKey should survive round-trip.
	if reconstructed[1].EventType != EventTypeScopeAcquired {
		t.Errorf("expected ScopeAcquired event type, got %s", reconstructed[1].EventType)
	}
	if reconstructed[1].ScopeKey != "vo:order:123" {
		t.Errorf("expected ScopeKey='vo:order:123', got %q", reconstructed[1].ScopeKey)
	}
}

// ---------------------------------------------------------------------------
// CompactWorkflowHistory store-level edge cases
// ---------------------------------------------------------------------------

// TestCompactWorkflowHistory_OpenChildrenInState verifies that the compaction
// state saved by CompactWorkflowHistory contains correct open children info.
func TestCompactWorkflowHistory_OpenChildrenInState(t *testing.T) {
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-a", ChildInput: `{"x":1}`, RunID: "run-a"},
		{Step: 1, EventType: EventTypeChildWorkflow, ChildName: "child-b", ChildInput: `{"y":2}`, RunID: "run-b"},
		{Step: 2, EventType: EventTypeAwaitChild, RunID: "run-a", Response: `{"ok":true}`},
		{Step: 3, EventType: EventTypeCall, Service: "svc", Op: "final"},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-children", threshold)
	if err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 compaction, got %d", store.compactCount)
	}

	var cs CompactionState
	if err := json.Unmarshal(store.compactState, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// keepStep = 4 - 1 = 3, so steps 0,1,2 are compacted
	// Step 2 is await_child for run-a, so child-b should be the only open child
	if len(cs.OpenChildren) != 1 {
		t.Errorf("expected 1 open child (child-b), got %d", len(cs.OpenChildren))
	}
	if len(cs.Events) != 3 {
		t.Errorf("expected 3 events in state, got %d", len(cs.Events))
	}
}

// TestCompactWorkflowHistory_DefersInState verifies that defers are captured
// in the compaction state by CompactWorkflowHistory.
func TestCompactWorkflowHistory_DefersInState(t *testing.T) {
	threshold := 2
	events := []EventRecord{
		{Step: 0, EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup"},
		{Step: 1, EventType: EventTypeDefer, DeferID: "d2", DeferDescription: "close db"},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "work"},
	}
	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-defers", threshold)
	if err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 compaction, got %d", store.compactCount)
	}

	var cs CompactionState
	if err := json.Unmarshal(store.compactState, &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// keepStep = 3 - 1 = 2, steps 0,1 are compacted
	if len(cs.PendingDefers) != 2 {
		t.Errorf("expected 2 pending defers in state, got %d", len(cs.PendingDefers))
	}
}

// ---------------------------------------------------------------------------
// Compaction edge cases
// ---------------------------------------------------------------------------

// TestCompactionWithOpenChildrenRoundTrip verifies that a workflow with child
// workflows still running survives a compaction round-trip correctly. The
// compaction state should track open children and reconstruct them faithfully.
func TestCompactionWithOpenChildrenRoundTrip(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-a", ChildInput: `{"x":1}`, RunID: "run-a"},
		{Step: 1, EventType: EventTypeChildWorkflow, ChildName: "child-b", ChildInput: `{"y":2}`, RunID: "run-b"},
		{Step: 2, EventType: EventTypeAwaitChild, RunID: "run-a", Response: `{"ok":true}`},
		// child-b is still open (no await or await_all for run-b)
	}

	cs := extractCompactionState(events)
	if len(cs.OpenChildren) != 1 {
		t.Fatalf("expected 1 open child (child-b), got %d", len(cs.OpenChildren))
	}
	if cs.OpenChildren[0].RunID != "run-b" {
		t.Errorf("expected open child run-b, got %s", cs.OpenChildren[0].RunID)
	}
	if cs.OpenChildren[0].Name != "child-b" {
		t.Errorf("expected open child name child-b, got %s", cs.OpenChildren[0].Name)
	}

	// Round-trip through buildFullHistoryFromCompaction.
	tail := []EventRecord{
		{Step: 3, EventType: EventTypeCall, Service: "svc", Op: "continue"},
	}
	reconstructed := buildFullHistoryFromCompaction(tail, cs)
	if len(reconstructed) != len(events)+len(tail) {
		t.Fatalf("expected %d events, got %d", len(events)+len(tail), len(reconstructed))
	}

	// Verify the child workflow events survived.
	for i, ev := range events {
		if !eventFieldsMatch(ev, reconstructed[i]) {
			t.Errorf("event %d (%s) round-trip mismatch", i, ev.EventType)
			dumpEventDiff(t, ev, reconstructed[i])
		}
	}
}

// TestCompactionWithPendingSignals verifies that signal_received events in
// the compacted portion of history are correctly preserved through compaction.
// Signals delivered before the compaction point should be available on replay.
func TestCompactionWithPendingSignals(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: "payment,approval", TimeoutMs: 30000},
		{Step: 1, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true,"amount":5000}`},
		{Step: 2, EventType: EventTypeAwaitSignals, SignalNames: "approval", TimeoutMs: 60000},
	}

	cs := extractCompactionState(events)
	reconstructed := buildFullHistoryFromCompaction(nil, cs)

	if len(reconstructed) != len(events) {
		t.Fatalf("expected %d reconstructed events, got %d", len(events), len(reconstructed))
	}

	// Verify signal events survived round-trip.
	if reconstructed[0].EventType != EventTypeAwaitSignals {
		t.Errorf("expected await_signals at [0], got %s", reconstructed[0].EventType)
	}
	if reconstructed[0].SignalNames != "payment,approval" {
		t.Errorf("expected SignalNames='payment,approval', got %q", reconstructed[0].SignalNames)
	}
	if reconstructed[1].EventType != EventTypeSignalReceived {
		t.Errorf("expected signal_received at [1], got %s", reconstructed[1].EventType)
	}
	if reconstructed[1].SignalName != "payment" {
		t.Errorf("expected SignalName='payment', got %q", reconstructed[1].SignalName)
	}
	if reconstructed[1].SignalPayload != `{"paid":true,"amount":5000}` {
		t.Errorf("expected SignalPayload with payment data, got %q", reconstructed[1].SignalPayload)
	}
	if reconstructed[2].EventType != EventTypeAwaitSignals {
		t.Errorf("expected await_signals at [2], got %s", reconstructed[2].EventType)
	}
}

// TestCompactionThresholdBoundaryExact verifies that exactly threshold events
// are NOT compacted, confirming the boundary condition in CompactWorkflowHistory.
func TestCompactionThresholdBoundaryExact(t *testing.T) {
	// Exactly 10 events with threshold=10 should NOT compact.
	threshold := 10
	events := make([]EventRecord, threshold)
	for i := 0; i < threshold; i++ {
		events[i] = EventRecord{
			Step: i, EventType: EventTypeCall,
			Service: "svc", Op: fmt.Sprintf("op%d", i),
			Request: `{}`, Response: fmt.Sprintf(`{"step":%d}`, i),
		}
	}

	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-boundary", threshold)
	if err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	if store.compactCount != 0 {
		t.Errorf("expected 0 compactions at exact threshold (%d events, threshold=%d), got %d",
			len(events), threshold, store.compactCount)
	}
}

// TestCompactionThresholdBoundaryPlusOne verifies that threshold+1 events
// triggers compaction (boundary + 1).
func TestCompactionThresholdBoundaryPlusOne(t *testing.T) {
	threshold := 10
	events := make([]EventRecord, threshold+1)
	for i := 0; i < threshold+1; i++ {
		events[i] = EventRecord{
			Step: i, EventType: EventTypeCall,
			Service: "svc", Op: fmt.Sprintf("op%d", i),
		}
	}

	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-boundary-plus", threshold)
	if err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	if store.compactCount != 1 {
		t.Errorf("expected 1 compaction for threshold+1 events (%d events, threshold=%d), got %d",
			len(events), threshold, store.compactCount)
	}
	// keepStep = (threshold+1) - threshold/2 = 11 - 5 = 6
	expectedKeepStep := (threshold + 1) - threshold/2
	if store.keepStep != expectedKeepStep {
		t.Errorf("expected keepStep=%d, got %d", expectedKeepStep, store.keepStep)
	}
}

// TestCompactionThresholdBoundaryLargeThreshold verifies compaction behavior
// with a large threshold to ensure the threshold/2 computation is correct.
func TestCompactionThresholdBoundaryLargeThreshold(t *testing.T) {
	threshold := 1000
	events := make([]EventRecord, threshold+100) // 1100 > 1000 → should compact
	for i := 0; i < threshold+100; i++ {
		events[i] = EventRecord{
			Step: i, EventType: EventTypeCall,
			Service: "svc", Op: fmt.Sprintf("op%d", i),
		}
	}

	store := &mockCompactStore{events: events}
	err := CompactWorkflowHistory(context.Background(), store, "wf-large", threshold)
	if err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}
	if store.compactCount != 1 {
		t.Fatalf("expected 1 compaction, got %d", store.compactCount)
	}
	// keepStep = 1100 - 500 = 600
	expectedKeepStep := len(events) - threshold/2
	if store.keepStep != expectedKeepStep {
		t.Errorf("expected keepStep=%d, got %d", expectedKeepStep, store.keepStep)
	}
	if len(store.compactState) == 0 {
		t.Fatal("expected non-empty compaction state")
	}
}
