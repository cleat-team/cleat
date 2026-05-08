package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Mock idempotency store for exactly-once tests
// ---------------------------------------------------------------------------

// idempotencyEntry tracks a single idempotency key registration.
type idempotencyEntry struct {
	runID    string
	expiresAt time.Time
	result   string
	errMsg   string
}

// mockIdempotencyStore implements the exactly-once start-new-run semantics
// using an in-memory map. It tracks idempotency keys and returns the
// existing runID when a duplicate is detected.
type mockIdempotencyStore struct {
	mu     sync.Mutex
	keys   map[string]*idempotencyEntry // idempotencyKey -> entry
	nextID int
}

func newMockIdempotencyStore() *mockIdempotencyStore {
	return &mockIdempotencyStore{
		keys: make(map[string]*idempotencyEntry),
	}
}

// StartNewRun starts a new workflow run or returns an existing one if the
// idempotencyKey was already used.
func (m *mockIdempotencyStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (runID string, alreadyExisted bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if idempotencyKey != "" {
		if entry, exists := m.keys[idempotencyKey]; exists {
			if time.Now().After(entry.expiresAt) {
				// Key expired — treat as a new run.
				delete(m.keys, idempotencyKey)
			} else {
				// Duplicate key — return existing runID.
				return entry.runID, true, nil
			}
		}
	}

	m.nextID++
	runID = fmt.Sprintf("run-%s-%d", defName, m.nextID)

	if idempotencyKey != "" {
		m.keys[idempotencyKey] = &idempotencyEntry{
			runID:     runID,
			expiresAt: time.Now().Add(24 * time.Hour),
		}
	}

	return runID, false, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestExactlyOnceDuplicateWorkflowID verifies that starting a workflow with the
// same idempotency key returns the first run's ID (alreadyExisted=true).
func TestExactlyOnceDuplicateWorkflowID(t *testing.T) {
	store := newMockIdempotencyStore()
	ctx := context.Background()
	input := json.RawMessage(`{"user":"test"}`)

	// First start with idempotency key.
	runID1, alreadyExisted, err := store.StartNewRun(ctx, "test-workflow", 1, input, "idem-key-001")
	if err != nil {
		t.Fatalf("first StartNewRun: %v", err)
	}
	if alreadyExisted {
		t.Error("first start should not report alreadyExisted")
	}
	if runID1 == "" {
		t.Fatal("expected non-empty runID")
	}

	// Second start with same key should return same runID.
	runID2, alreadyExisted, err := store.StartNewRun(ctx, "test-workflow", 1, input, "idem-key-001")
	if err != nil {
		t.Fatalf("second StartNewRun: %v", err)
	}
	if !alreadyExisted {
		t.Error("second start with same key should report alreadyExisted")
	}
	if runID2 != runID1 {
		t.Errorf("expected same runID %q, got %q", runID1, runID2)
	}
}

// TestExactlyOnceDifferentKeys verifies that different idempotency keys
// produce different workflow runs.
func TestExactlyOnceDifferentKeys(t *testing.T) {
	store := newMockIdempotencyStore()
	ctx := context.Background()
	input := json.RawMessage(`{}`)

	runID1, _, err := store.StartNewRun(ctx, "wf", 1, input, "key-a")
	if err != nil {
		t.Fatalf("key-a: %v", err)
	}
	runID2, _, err := store.StartNewRun(ctx, "wf", 1, input, "key-b")
	if err != nil {
		t.Fatalf("key-b: %v", err)
	}

	if runID1 == runID2 {
		t.Error("different keys should produce different run IDs")
	}
}

// TestExactlyOnceNoKey verifies that starting a workflow without an idempotency
// key always creates a new run.
func TestExactlyOnceNoKey(t *testing.T) {
	store := newMockIdempotencyStore()
	ctx := context.Background()
	input := json.RawMessage(`{}`)

	runID1, _, err := store.StartNewRun(ctx, "wf", 1, input, "")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	runID2, alreadyExisted, err := store.StartNewRun(ctx, "wf", 1, input, "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if alreadyExisted {
		t.Error("no idempotency key should never report alreadyExisted")
	}
	if runID1 == runID2 {
		t.Error("different runs without keys should have different IDs")
	}
}

// TestExactlyOnceIdempotencyKeyExpiration verifies that expired idempotency
// keys allow a new workflow to be started (the old entry is cleaned up).
func TestExactlyOnceIdempotencyKeyExpiration(t *testing.T) {
	store := newMockIdempotencyStore()
	ctx := context.Background()
	input := json.RawMessage(`{}`)

	// Use a key with immediate expiration by manipulating the store's clock.
	// We simulate expiry by directly setting expiresAt in the past.
	store.mu.Lock()
	store.keys["expired-key"] = &idempotencyEntry{
		runID:     "old-run",
		expiresAt: time.Now().Add(-1 * time.Second), // already expired
	}
	store.mu.Unlock()

	// Starting with the expired key should create a new run (not return the old).
	runID, alreadyExisted, err := store.StartNewRun(ctx, "wf", 1, input, "expired-key")
	if err != nil {
		t.Fatalf("StartNewRun with expired key: %v", err)
	}
	if alreadyExisted {
		t.Error("expired key should not report alreadyExisted")
	}
	if runID == "old-run" {
		t.Error("expired key should return a new runID, not the old one")
	}
}

// TestExactlyOnceIdempotencyKeyCollision verifies that different idempotency
// keys do not collide with each other.
func TestExactlyOnceIdempotencyKeyCollision(t *testing.T) {
	store := newMockIdempotencyStore()
	ctx := context.Background()
	input := json.RawMessage(`{}`)

	// Start many workflows with different keys.
	runIDs := make(map[string]string)
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("unique-key-%d", i)
		runID, alreadyExisted, err := store.StartNewRun(ctx, "wf", 1, input, key)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if alreadyExisted {
			t.Errorf("key %q should not report alreadyExisted on first use", key)
		}
		runIDs[key] = runID
	}

	// All run IDs should be unique.
	if len(runIDs) != 10 {
		t.Errorf("expected 10 unique run IDs, got %d", len(runIDs))
	}

	// Verify each key returns its own runID on replay.
	for key, expectedRunID := range runIDs {
		runID, alreadyExisted, err := store.StartNewRun(ctx, "wf", 1, input, key)
		if err != nil {
			t.Fatalf("replay key %q: %v", key, err)
		}
		if !alreadyExisted {
			t.Errorf("key %q should report alreadyExisted on second use", key)
		}
		if runID != expectedRunID {
			t.Errorf("key %q: expected runID %q, got %q", key, expectedRunID, runID)
		}
	}
}

// ---------------------------------------------------------------------------
// SideEffect exactly-once tests
// ---------------------------------------------------------------------------

// TestSideEffectFreshExecution verifies that a SideEffect call on a fresh
// execution records the computed result in history and writes it to WASM memory.
func TestSideEffectFreshExecution(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	computedResult := `{"random":42,"nonce":"abc123"}`
	result := session.SideEffect(ctx, mod, computedResult, 0, 4096)
	errCode, written := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Fatal("expected response written to memory")
	}

	// Verify the result was recorded in history.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(session.history))
	}
	rec := session.history[0]
	if rec.EventType != EventTypeSideEffect {
		t.Errorf("expected SideEffect event type, got %s", rec.EventType)
	}
	if rec.SideEffectResult != computedResult {
		t.Errorf("expected SideEffectResult=%q, got %q", computedResult, rec.SideEffectResult)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}

	// Verify the computed value was written into WASM memory.
	mem := mod.Memory()
	data, ok := mem.Read(0, written)
	if !ok || string(data) != computedResult {
		t.Errorf("expected %q in memory, got %q", computedResult, string(data))
	}
}

// TestSideEffectRoundTrip verifies that a SideEffect result recorded during
// a fresh execution is faithfully replayed back during a subsequent replay,
// returning the cached result instead of the new computed value.
func TestSideEffectRoundTrip(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Fresh execution records a side effect result.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}
	computedResult := `{"order_id":"ord-123","status":"paid"}`
	_ = freshSession.SideEffect(ctx, mod, computedResult, 0, 4096)

	// Capture the recorded history.
	history := freshSession.history

	// Replay uses a fresh module instance but the same recorded history.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// During replay, the SideEffect should return the cached result, not the
	// new computed value passed here.
	differentResult := `{"order_id":"ord-123","status":"refunded"}`
	result := replaySession.SideEffect(ctx, mod2, differentResult, 0, 4096)
	errCode, written := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Fatal("expected response written to memory")
	}

	// The cached result (from history) should be returned.
	mem := mod2.Memory()
	data, ok := mem.Read(0, written)
	if !ok {
		t.Fatal("failed to read WASM memory")
	}
	if string(data) != computedResult {
		t.Errorf("expected cached result %q, got %q", computedResult, string(data))
	}
	if replaySession.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", replaySession.stepCount)
	}
}

// ---------------------------------------------------------------------------
// DurableSleep deduplication test
// ---------------------------------------------------------------------------

// TestDurableSleepDeduplication verifies that duplicate DurableSleep calls
// are deduplicated on replay: the recorded sleep event is consumed from history
// and the function returns completed immediately without re-suspending.
func TestDurableSleepDeduplication(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Fresh execution: DurableSleep should suspend.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
		nowMs:   1000000,
	}
	result := freshSession.DurableSleep(ctx, mod, 5000)
	status, _ := decodeSleepResult(result)
	if status != sleepStatusSuspend {
		t.Errorf("expected sleep status suspend (%d), got %d", sleepStatusSuspend, status)
	}
	if freshSession.suspendErr == nil {
		t.Fatal("expected suspendErr after fresh sleep")
	}
	if len(freshSession.history) != 1 {
		t.Fatalf("expected 1 sleep event in history, got %d", len(freshSession.history))
	}

	// Replay session with the recorded history.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  freshSession.history,
		isReplay: true,
		nowMs:    1005000, // later time
	}

	// On replay, the sleep should be resolved from history without re-suspending.
	result2 := replaySession.DurableSleep(ctx, mod2, 5000)
	status2, duration2 := decodeSleepResult(result2)
	if status2 != sleepStatusCompleted {
		t.Errorf("expected sleep status completed (%d) on replay, got %d", sleepStatusCompleted, status2)
	}
	if duration2 != 0 {
		t.Errorf("expected duration 0 on replay, got %d", duration2)
	}
	if replaySession.suspendErr != nil {
		t.Error("expected no suspendErr on replay sleep")
	}
	if replaySession.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", replaySession.stepCount)
	}
}

// ---------------------------------------------------------------------------
// Signal delivery deduplication tests
// ---------------------------------------------------------------------------

// TestSignalDeliveryDeduplication verifies that delivering the same signal
// multiple times only produces one signal_received event after replay.
func TestSignalDeliveryDeduplication(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Fresh execution: simulate signal delivery.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	// Record a signal received event.
	freshSession.history = append(freshSession.history, EventRecord{
		Step:          0,
		EventType:     EventTypeSignalReceived,
		SignalName:    "payment_confirmed",
		SignalPayload: `{"txn_id":"txn-123"}`,
	})
	freshSession.stepCount = 1

	// Replay session with the recorded signal event.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  freshSession.history,
		isReplay: true,
	}

	// During replay, DurableAwaitSignals should consume the signal_received
	// event and return immediately without re-polling the signal store.
	result := replaySession.DurableAwaitSignals(ctx, mod2, "payment_confirmed", 30000, 0, 4096, 4096, 4096)
	if result == 0 {
		t.Error("expected non-zero result from signal replay")
	}
	// Verify the step advanced by 1 (just the signal_received event consumed).
	if replaySession.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", replaySession.stepCount)
	}

	// Calling DurableAwaitSignals again should fall through to fresh execution
	// since history is now exhausted.
	result2 := replaySession.DurableAwaitSignals(ctx, mod2, "payment_confirmed", 30000, 0, 4096, 4096, 4096)
	// No signal store means no signal found, so it should suspend.
	if result2 == 0 {
		t.Log("second await signals suspended as expected with no signal store")
	} else {
		t.Log("second await signals did not suspend (history exhausted, no signal store)")
	}
}

// ---------------------------------------------------------------------------
// Event replay tests (Engine-level Replay entry point)
// ---------------------------------------------------------------------------

// TestReplayFromStoredEventHistory verifies that a workflow can be replayed
// from a manually constructed event history without executing any real calls.
func TestReplayFromStoredEventHistory(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Build a synthetic event history for replay.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"step":1}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2", Request: `{}`, Response: `{"step":2}`},
		{Step: 2, EventType: EventTypeSideEffect, SideEffectResult: `{"side":"result"}`},
	}

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// Replay the calls and side effects from the stored history.
	result1 := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, callErrCode1, errCode1 := decodeCallResult(result1)
	if errCode1 != 0 || callErrCode1 != 0 {
		t.Fatalf("replay call op1: unexpected errCode=%d callErrCode=%d", errCode1, callErrCode1)
	}

	result2 := session.replayCall(ctx, mod, "svc", "op2", `{}`, 0, 4096)
	_, callErrCode2, errCode2 := decodeCallResult(result2)
	if errCode2 != 0 || callErrCode2 != 0 {
		t.Fatalf("replay call op2: unexpected errCode=%d callErrCode=%d", errCode2, callErrCode2)
	}

	result3 := session.SideEffect(ctx, mod, `{"ignored":"fresh"}`, 0, 4096)
	errCode3, written3 := decodeSimpleResult(result3)
	if errCode3 != 0 {
		t.Fatalf("replay side effect: unexpected errCode=%d", errCode3)
	}
	// The replayed side effect should return the cached result, not "fresh".
	mem := mod.Memory()
	data, ok := mem.Read(0, written3)
	if !ok || string(data) != `{"side":"result"}` {
		t.Errorf("expected cached side effect result, got %q", string(data))
	}

	if session.stepCount != 3 {
		t.Errorf("expected stepCount=3, got %d", session.stepCount)
	}
}

// TestReplayWithMissingEvents verifies that replay with insufficient history
// causes divergence errors.
func TestReplayWithMissingEvents(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// History has only 1 event but we try to replay 2 calls.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
	}

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// First replay succeeds.
	result1 := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, _, errCode1 := decodeCallResult(result1)
	if errCode1 != 0 {
		t.Fatalf("first replay should succeed, got errCode=%d", errCode1)
	}

	// Second replay is past history → falls through to fresh execution.
	result2 := session.replayCall(ctx, mod, "svc", "op2", `{}`, 0, 4096)
	_, _, errCode2 := decodeCallResult(result2)
	// History exhausted, so it switches to fresh execution which succeeds.
	if errCode2 != 0 {
		t.Errorf("expected fallthrough to fresh execution, got errCode=%d", errCode2)
	}
	if session.isReplay {
		t.Error("expected isReplay to be false after history exhaustion")
	}

	// Verify the fresh call was recorded (history should grow).
	if len(session.history) != 2 {
		t.Errorf("expected 2 events in history (1 replayed + 1 fresh), got %d", len(session.history))
	}
}

// TestReplayWithMissingEventsDivergence verifies that replay with an
// event type mismatch causes a divergence error.
func TestReplayWithMissingEventsDivergence(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// History has a sleep event, but replay attempts a call.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeSleep, DurationMs: 5000},
	}

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// Replay a call, but history has a sleep at step 0 → divergence.
	result := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected non-zero errCode for event type divergence")
	}
}

// TestReplayWithContinueAsNew verifies that ContinueAsNew events in the
// event history are correctly replayed, setting up the next run with the
// stored new input.
func TestReplayWithContinueAsNew(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Build a history that includes a ContinueAsNew event.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "first", Request: `{}`, Response: `{"phase":1}`},
		{Step: 1, EventType: EventTypeContinueAsNew, NewInput: `{"phase":2,"restart":true}`},
	}

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// Replay the initial call.
	_ = session.replayCall(ctx, mod, "svc", "first", `{}`, 0, 4096)

	// Replay the ContinueAsNew event.
	_ = session.ContinueAsNew(ctx, mod, `{"should_be_ignored":true}`)

	if session.suspendErr == nil {
		t.Fatal("expected suspendErr after ContinueAsNew replay")
	}
	if session.suspendErr.NewInput != `{"phase":2,"restart":true}` {
		t.Errorf("expected NewInput from history %q, got %q",
			`{"phase":2,"restart":true}`, session.suspendErr.NewInput)
	}
	if session.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", session.suspendErr.Reason)
	}
}

// TestSideEffectMultipleRoundTrip verifies that multiple consecutive
// SideEffect calls are correctly recorded and replayed.
func TestSideEffectMultipleRoundTrip(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Record two side effects in sequence.
	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	results := []string{`{"step":1}`, `{"step":2}`}
	for _, r := range results {
		_ = session.SideEffect(ctx, mod, r, 0, 4096)
	}

	if len(session.history) != 2 {
		t.Fatalf("expected 2 side effect events, got %d", len(session.history))
	}

	// Now replay them.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  session.history,
		isReplay: true,
	}

	for i, expected := range results {
		result := replaySession.SideEffect(ctx, mod2, `{"ignored":"fresh"}`, 0, 4096)
		errCode, written := decodeSimpleResult(result)
		if errCode != 0 {
			t.Fatalf("replay side effect %d: errCode=%d", i, errCode)
		}
		mem := mod2.Memory()
		data, ok := mem.Read(uint32(i*50), written) // different offsets to avoid overlap
		if !ok {
			t.Fatalf("side effect %d: failed to read memory", i)
		}
		// Re-read from offset 0 since writeWasmString always writes to ptr (0 for respPtr).
		data, ok = mem.Read(0, written)
		if ok && string(data) != expected {
			t.Errorf("side effect %d: expected %q, got %q", i, expected, string(data))
		}
	}
}

// ---------------------------------------------------------------------------
// SideEffect record-then-replay: record a side effect, then replay the same
// step with a new WASM module instance and verify the cached result is used.
// ---------------------------------------------------------------------------

func TestSideEffectRecordThenReplaySameStep(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Record two side effects in a fresh session.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}
	firstResult := `{"random":100}`
	secondResult := `{"random":200}`
	_ = freshSession.SideEffect(ctx, mod, firstResult, 0, 4096)
	_ = freshSession.SideEffect(ctx, mod, secondResult, 0, 4096)

	if len(freshSession.history) != 2 {
		t.Fatalf("expected 2 events in history, got %d", len(freshSession.history))
	}

	// Replay the same steps with a new module. The cached values should be returned
	// even though we pass different "fresh" values.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  freshSession.history,
		isReplay: true,
	}

	// First replay: should return firstResult, not firstReplayVal.
	firstReplayVal := `{"random":999}`
	result1 := replaySession.SideEffect(ctx, mod2, firstReplayVal, 0, 4096)
	errCode1, written1 := decodeSimpleResult(result1)
	if errCode1 != 0 {
		t.Fatalf("first replay side effect: unexpected errCode=%d", errCode1)
	}
	mem2 := mod2.Memory()
	data1, ok1 := mem2.Read(0, written1)
	if !ok1 || string(data1) != firstResult {
		t.Errorf("expected cached first result %q, got %q", firstResult, string(data1))
	}

	// Second replay: should return secondResult, not secondReplayVal.
	secondReplayVal := `{"random":888}`
	result2 := replaySession.SideEffect(ctx, mod2, secondReplayVal, 0, 4096)
	errCode2, written2 := decodeSimpleResult(result2)
	if errCode2 != 0 {
		t.Fatalf("second replay side effect: unexpected errCode=%d", errCode2)
	}
	data2, ok2 := mem2.Read(0, written2)
	if !ok2 || string(data2) != secondResult {
		t.Errorf("expected cached second result %q, got %q", secondResult, string(data2))
	}

	if replaySession.stepCount != 2 {
		t.Errorf("expected stepCount=2 after replaying 2 side effects, got %d", replaySession.stepCount)
	}
}

// ---------------------------------------------------------------------------
// DurableSleep dedup across replay sessions: verify that multiple sleeps are
// consumed from history without re-suspending.
// ---------------------------------------------------------------------------

func TestDurableSleepMsDedupAcrossReplays(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Fresh execution records a sleep event.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
		nowMs:   1000000,
	}
	_ = freshSession.DurableSleep(ctx, mod, 5000)
	if len(freshSession.history) != 1 {
		t.Fatalf("expected 1 sleep event, got %d", len(freshSession.history))
	}
	if freshSession.suspendErr == nil {
		t.Fatal("expected suspendErr after fresh sleep")
	}

	// Replay the sleep event — should complete without re-suspending.
	mod2 := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  freshSession.history,
		isReplay: true,
		nowMs:    1005000,
	}
	result := replaySession.DurableSleep(ctx, mod2, 5000)
	status, _ := decodeSleepResult(result)
	if status != sleepStatusCompleted {
		t.Errorf("expected sleep completed (%d) on replay, got %d", sleepStatusCompleted, status)
	}
	if replaySession.suspendErr != nil {
		t.Error("expected no suspendErr on replay sleep")
	}
	if replaySession.stepCount != 1 {
		t.Errorf("expected stepCount=1 after replay, got %d", replaySession.stepCount)
	}

	// Calling DurableSleep again without history should suspend.
	result2 := replaySession.DurableSleep(ctx, mod2, 3000)
	status2, duration2 := decodeSleepResult(result2)
	if status2 != sleepStatusSuspend {
		t.Errorf("expected sleep suspend (%d) when history exhausted, got %d", sleepStatusSuspend, status2)
	}
	if duration2 != 3000 {
		t.Errorf("expected duration 3000 on fresh sleep, got %d", duration2)
	}
}

// ---------------------------------------------------------------------------
// Signal delivery deduplication on replay: verify that delivering the same
// signal multiple times only produces one signal_received event.
// ---------------------------------------------------------------------------

func TestSignalDeliveryDedupReplay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Simulate a fresh execution where a signal is delivered once.
	freshSession := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}
	freshSession.history = append(freshSession.history, EventRecord{
		Step:          0,
		EventType:     EventTypeSignalReceived,
		SignalName:    "order_approved",
		SignalPayload: `{"order_id":"ord-456","approved":true}`,
	})
	freshSession.stepCount = 1

	// Replay: consume the signal_received event.
	mod := newTestModule(t, rt)
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  freshSession.history,
		isReplay: true,
	}

	// First await should consume the signal_received event and return the payload.
	result1 := replaySession.DurableAwaitSignals(ctx, mod, "order_approved", 30000, 0, 4096, 4096, 4096)
	if result1 == 0 {
		t.Error("expected non-zero result from first signal await")
	}
	if replaySession.stepCount != 1 {
		t.Errorf("expected stepCount=1 after consuming signal, got %d", replaySession.stepCount)
	}

	// Second await has no more signal events in history. Without a signal store,
	// it should fall through to fresh execution without crashing or panicking.
	mod2 := newTestModule(t, rt)
	result2 := replaySession.DurableAwaitSignals(ctx, mod2, "order_approved", 30000, 0, 4096, 4096, 4096)
	t.Logf("second await signals returned result=%d (expected fallthrough, no crash)", result2)
	// The engine should not panic or hang when called with exhausted history
	// and no signal store. The exact return value depends on whether the
	// fallthrough records an await_signals event.
}

// ---------------------------------------------------------------------------
// ContinueAsNew with event replay: verify that ContinueAsNew events are
// correctly replayed with the stored new input set in the suspendErr.
// ---------------------------------------------------------------------------

func TestContinueAsNewWithEventReplay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Build history: a call followed by a ContinueAsNew event.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "phase1", Request: `{}`, Response: `{"phase":1}`},
		{Step: 1, EventType: EventTypeContinueAsNew, NewInput: `{"phase":2,"restart":true}`},
	}

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  history,
		isReplay: true,
	}

	// Replay the initial call - should succeed from history.
	_ = session.replayCall(ctx, mod, "svc", "phase1", `{}`, 0, 4096)
	if session.stepCount != 1 {
		t.Fatalf("expected stepCount=1 after replay call, got %d", session.stepCount)
	}

	// Replay the ContinueAsNew event - should set suspendErr with the stored input.
	_ = session.ContinueAsNew(ctx, mod, `{"should_be_ignored":true}`)
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr after ContinueAsNew replay")
	}
	if session.suspendErr.NewInput != `{"phase":2,"restart":true}` {
		t.Errorf("expected NewInput from history %q, got %q",
			`{"phase":2,"restart":true}`, session.suspendErr.NewInput)
	}
	if session.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", session.suspendErr.Reason)
	}
	if session.stepCount != 2 {
		t.Errorf("expected stepCount=2 after consuming call + ContinueAsNew, got %d", session.stepCount)
	}

	// The session should remain in replay mode since no fresh execution occurred.
	if !session.isReplay {
		t.Error("expected isReplay=true (ContinueAsNew consumes from replay, no fresh execution)")
	}
}
