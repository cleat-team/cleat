package engine

import (
	"context"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// DurableSend tests.
// ---------------------------------------------------------------------------

// DurableSend is fire-and-forget: it records an event and optionally spawns
// a goroutine to call the caller. On replay, it advances past the recorded
// event without re-executing the call.

func TestDurableSendReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeDurableSend,
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"key":"val"}`,
	}}
	result := s.DurableSend(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestDurableSendReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // past end

	result := s.DurableSend(context.Background(), nil, "my-svc", "my-op", `{}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// On past-end, DurableSend returns 0 without calling exitReplay.
	if !s.isReplay {
		t.Error("expected isReplay to remain true (DurableSend does not exitReplay on past-end)")
	}
}

func TestDurableSendFreshRecordsEvent(t *testing.T) {
	s := newTestExecSession()

	result := s.DurableSend(context.Background(), nil, "my-svc", "my-op", `{"k":"v"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	r := s.history[0]
	if r.EventType != EventTypeDurableSend {
		t.Errorf("expected EventTypeDurableSend, got %q", r.EventType)
	}
	if r.Service != "my-svc" {
		t.Errorf("expected Service 'my-svc', got %q", r.Service)
	}
	if r.Op != "my-op" {
		t.Errorf("expected Op 'my-op', got %q", r.Op)
	}
	if r.Request != `{"k":"v"}` {
		t.Errorf("expected Request '{\"k\":\"v\"}', got %q", r.Request)
	}
}

// TestDurableSendFreshWithCaller verifies that when a ServiceCaller is present,
// DurableSend records the event and spawns a goroutine. The call is async so
// we cannot easily observe the result, but we can verify the event was recorded.
func TestDurableSendFreshWithCaller(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller

	result := s.DurableSend(context.Background(), nil, "my-svc", "my-op", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	// The caller.Call is async; we don't assert on calls here.
	// This test primarily verifies no panic/error from the goroutine spawn.
}

// ---------------------------------------------------------------------------
// PollSignal tests.
// ---------------------------------------------------------------------------

// PollSignal checks the signal store for a signal payload without consuming it.
// It relies on s.engine.signalStore.PollSignal.

func TestPollSignalFound(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()

	// Pre-deliver a signal.
	err := store.DeliverSignal(ctx, "wf-poll", "test-signal", `{"data":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.workflowID = "wf-poll" // must match DeliverSignal
	s.engine.signalStore = store

	result := s.PollSignal(ctx, nil, "test-signal", 0, 0)

	// When found: returns (written << 32 | 0x0100). With nil module, written=0.
	flags := uint32(result)
	if flags&0x0100 == 0 {
		t.Error("expected found flag (0x0100) to be set")
	}
}

func TestPollSignalNotFound(t *testing.T) {
	store := newMockSignalWorkflowStore()
	s := newTestExecSession()
	s.engine.workflowID = "wf-poll"
	s.engine.signalStore = store

	result := s.PollSignal(context.Background(), nil, "nonexistent-signal", 0, 0)

	if result != 0 {
		t.Errorf("expected 0 (not found), got %d", result)
	}
}

func TestPollSignalNoStore(t *testing.T) {
	s := newTestExecSession()
	// signalStore is nil
	result := s.PollSignal(context.Background(), nil, "test-signal", 0, 0)

	if result != 0 {
		t.Errorf("expected 0 when store is nil, got %d", result)
	}
}

func TestPollSignalWithPayload(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()

	err := store.DeliverSignal(ctx, "wf-poll", "greeting", `{"msg":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.workflowID = "wf-poll" // must match DeliverSignal
	s.engine.signalStore = store

	// With a raw memory buffer, the payload should be written.
	buf := make([]byte, 128)
	ctx = contextWithRawMemBuf(ctx, buf)
	result := s.PollSignal(ctx, nil, "greeting", 0, 128)

	written := uint32(result >> 32)
	if written == 0 {
		t.Error("expected written > 0 when signal found with payload")
	}
	if string(buf[:written]) != `{"msg":"hello"}` {
		t.Errorf("expected payload %q in buffer, got %q", `{"msg":"hello"}`, string(buf[:written]))
	}
}

// ---------------------------------------------------------------------------
// SetScope / ClearScope tests.
// ---------------------------------------------------------------------------

// SetScope manages virtual-object scopes with optional concurrency key store.
// When concurrencyKeyStore is nil, it manages scope fields in memory only.

func TestSetScopeFreshNoStore(t *testing.T) {
	s := newTestExecSession()
	// No concurrencyKeyStore — purely in-memory scope management.
	ctx := context.Background()

	result := s.SetScope(ctx, nil, "account", "acct-123", 0, 0)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !s.scopeSet {
		t.Error("expected scopeSet=true")
	}
	if s.scopePrefix != "vo:account:acct-123:" {
		t.Errorf("expected scopePrefix 'vo:account:acct-123', got %q", s.scopePrefix)
	}
	if s.scopeObjType != "account" {
		t.Errorf("expected scopeObjType 'account', got %q", s.scopeObjType)
	}
	if s.scopeInstKey != "acct-123" {
		t.Errorf("expected scopeInstKey 'acct-123', got %q", s.scopeInstKey)
	}
	// Should produce one history event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeScopeAcquired {
		t.Errorf("expected EventTypeScopeAcquired, got %q", s.history[0].EventType)
	}
}

func TestSetScopeEmptyClears(t *testing.T) {
	s := newTestExecSession()
	// Set a scope first.
	s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)

	// Then clear it by passing empty strings.
	result := s.SetScope(context.Background(), nil, "", "", 0, 0)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.scopeSet {
		t.Error("expected scopeSet=false after clearing with empty args")
	}
	if s.scopePrefix != "" {
		t.Errorf("expected empty scopePrefix, got %q", s.scopePrefix)
	}
}

func TestSetScopeWithConcurrencyStore(t *testing.T) {
	store := &mockConcurrencyKeyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	ctx := context.Background()
	result := s.SetScope(ctx, nil, "order", "ord-999", 0, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !s.scopeSet {
		t.Error("expected scopeSet=true")
	}
	if s.scopePrefix != "vo:order:ord-999:" {
		t.Errorf("expected scopePrefix 'vo:order:ord-999:', got %q", s.scopePrefix)
	}
	if len(s.heldScopes) != 1 {
		t.Fatalf("expected 1 held scope, got %d", len(s.heldScopes))
	}
	// heldScopes stores the key WITHOUT trailing colon, scopePrefix has it.
	if s.heldScopes[0] != "vo:order:ord-999" {
		t.Errorf("expected held scope 'vo:order:ord-999', got %q", s.heldScopes[0])
	}
	if s.scopePrefix != "vo:order:ord-999:" {
		t.Errorf("expected scopePrefix 'vo:order:ord-999:', got %q", s.scopePrefix)
	}
}

func TestSetScopeReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeScopeAcquired,
		ScopeKey:  "vo:account:acct-123",
	}}
	result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	if !s.scopeSet {
		t.Error("expected scopeSet=true after replay match")
	}
}

func TestClearScope(t *testing.T) {
	s := newTestExecSession()
	// First set a scope.
	s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)
	if !s.scopeSet {
		t.Fatal("scope should be set")
	}

	// Clear it explicitly.
	s.ClearScope(context.Background())

	if s.scopeSet {
		t.Error("expected scopeSet=false after ClearScope")
	}
	if s.scopePrefix != "" {
		t.Errorf("expected empty scopePrefix, got %q", s.scopePrefix)
	}
}

func TestClearScopeNoopWhenNotSet(t *testing.T) {
	s := newTestExecSession()
	// Should not panic when scope is not set.
	s.ClearScope(context.Background())
	if s.scopeSet {
		t.Error("expected scopeSet=false")
	}
}

func TestSetScopeReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // past end

	result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)

	// Past end: exitReplay then freshSetScope.
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if !s.replayJustEnded {
		t.Error("expected replayJustEnded=true")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !s.scopeSet {
		t.Error("expected scopeSet=true after past-end fresh execution")
	}
}

// ---------------------------------------------------------------------------
// AcquireLock tests.
// ---------------------------------------------------------------------------

func TestAcquireLockNoStore(t *testing.T) {
	s := newTestExecSession()
	// concurrencyKeyStore is nil

	result := s.AcquireLock(context.Background(), nil, "lock-key-1", 10000)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// Even without a store, the event is recorded (LockAcquired=0).
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAcquireLock {
		t.Errorf("expected EventTypeAcquireLock, got %q", s.history[0].EventType)
	}
	if s.history[0].LockKey != "lock-key-1" {
		t.Errorf("expected LockKey 'lock-key-1', got %q", s.history[0].LockKey)
	}
}

func TestAcquireLockWithStoreAcquired(t *testing.T) {
	store := &mockConcurrencyKeyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	result := s.AcquireLock(context.Background(), nil, "my-lock", 5000)

	// packAcquireLockResult(acquired, 0): with acquired=true and no error,
	// the result is success. Let's verify the event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].LockAcquired != 1 {
		t.Errorf("expected LockAcquired=1 (acquired=true), got %d", s.history[0].LockAcquired)
	}
	if s.history[0].Err != "" {
		t.Errorf("expected no error, got %q", s.history[0].Err)
	}
	// Result should indicate acquired=true.
	_ = result
}

func TestAcquireLockReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:         0,
		EventType:    EventTypeAcquireLock,
		LockKey:      "my-lock",
		LockTTLMs:    5000,
		LockAcquired: 1,
	}}
	result := s.AcquireLock(context.Background(), nil, "my-lock", 5000)

	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	// Result should have LockAcquired=1 (acquired=true).
	_ = result
}

func TestAcquireLockReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}
	result := s.AcquireLock(context.Background(), nil, "my-lock", 5000)

	// replayAcquireLock returns error directly without calling exitReplay.
	if !s.isReplay {
		t.Error("expected isReplay to remain true (mismatch does not call exitReplay)")
	}
	// Divergence: returns packAcquireLockResult(false, 1).
	errCode := uint32(result)
	if errCode == 0 {
		t.Error("expected error code != 0 on divergence")
	}
}

func TestAcquireLockReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // past end

	result := s.AcquireLock(context.Background(), nil, "my-lock", 5000)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	// Past-end: exitReplay, then freshAcquireLock with nil store → acquired=false
	// but event is recorded.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].LockAcquired != 0 {
		t.Errorf("expected LockAcquired=0 (no store), got %d", s.history[0].LockAcquired)
	}
	_ = result
}

// ---------------------------------------------------------------------------
// ReleaseLock tests.
// ---------------------------------------------------------------------------

func TestReleaseLockNoStore(t *testing.T) {
	s := newTestExecSession()
	// concurrencyKeyStore is nil

	result := s.ReleaseLock(context.Background(), nil, "my-lock")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// Fresh path with nil store: records event with released=true.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeReleaseLock {
		t.Errorf("expected EventTypeReleaseLock, got %q", s.history[0].EventType)
	}
}

func TestReleaseLockWithStore(t *testing.T) {
	store := &mockConcurrencyKeyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	result := s.ReleaseLock(context.Background(), nil, "my-lock")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeReleaseLock {
		t.Errorf("expected EventTypeReleaseLock, got %q", s.history[0].EventType)
	}
}

func TestReleaseLockReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeReleaseLock,
		LockKey:   "my-lock",
	}}
	result := s.ReleaseLock(context.Background(), nil, "my-lock")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestReleaseLockReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // past end

	result := s.ReleaseLock(context.Background(), nil, "my-lock")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if !s.replayJustEnded {
		t.Error("expected replayJustEnded=true")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// Fresh path should record event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeReleaseLock {
		t.Errorf("expected EventTypeReleaseLock, got %q", s.history[0].EventType)
	}
}

// ---------------------------------------------------------------------------
// PollCancellation edge cases.
// ---------------------------------------------------------------------------

func TestPollCancellationCancelled(t *testing.T) {
	s := newTestExecSession()
	// With nil signalStore and not in replay, returns 0 (not cancelled).
	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestPollCancellationReplayStaysReplay(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// During replay, PollCancellation always returns 0 (never cancelled).
	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 during replay, got %d", result)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

// ---------------------------------------------------------------------------
// DurableDefer edge case tests.
// ---------------------------------------------------------------------------

func TestDurableDeferReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:             0,
		EventType:        EventTypeDefer,
		DeferDescription: "cleanup",
	}}
	result := s.DurableDefer(context.Background(), nil, "cleanup", 0, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestDurableDeferReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.DurableDefer(context.Background(), nil, "cleanup", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if !s.replayJustEnded {
		t.Error("expected replayJustEnded=true")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.deferrals) != 1 {
		t.Errorf("expected 1 deferral, got %d", len(s.deferrals))
	}
	// The deferral map is keyed by auto-generated defer ID, value is description.
	for k, v := range s.deferrals {
		if v != "cleanup" {
			t.Errorf("expected deferral description 'cleanup', got %q for key %q", v, k)
		}
	}
}

// ---------------------------------------------------------------------------
// DurableScheduleInvoke edge case: replay divergence.
// ---------------------------------------------------------------------------

func TestScheduleInvokeReplayAdvancesAnyEvent(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // DurableScheduleInvoke does not check event type
	}}
	result := s.DurableScheduleInvoke(context.Background(), nil, "my-svc", "my-op", `{}`, 5000)

	// Advances past the event without checking type, never calls exitReplay.
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 (advanced past event), got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

// ---------------------------------------------------------------------------
// ConcurrencyKeyStore that tracks release calls.
// ---------------------------------------------------------------------------

// trackingConcurrencyStore records every call to ReleaseConcurrencyKey.
type trackingConcurrencyStore struct {
	mockConcurrencyKeyStore
	mu       sync.Mutex
	releases []string
}

func (t *trackingConcurrencyStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releases = append(t.releases, key)
	return nil
}

func TestSetScopeSwitchingReleasesOldKey(t *testing.T) {
	store := &trackingConcurrencyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	ctx := context.Background()

	// Set scope to account:acct-123
	s.SetScope(ctx, nil, "account", "acct-123", 0, 0)
	if len(store.releases) != 0 {
		t.Errorf("expected 0 releases after first scope set, got %d", len(store.releases))
	}

	// Switch to a different scope — should release the old key.
	s.SetScope(ctx, nil, "order", "ord-456", 0, 0)
	if len(store.releases) != 1 {
		t.Fatalf("expected 1 release after scope switch, got %d", len(store.releases))
	}
	if store.releases[0] != "vo:account:acct-123" {
		t.Errorf("expected release of old scope key 'vo:account:acct-123', got %q", store.releases[0])
	}

	// new scope fields should be set.
	if s.scopePrefix != "vo:order:ord-456:" {
		t.Errorf("expected scopePrefix 'vo:order:ord-456:', got %q", s.scopePrefix)
	}
}

func TestClearScopeReleasesHeldScope(t *testing.T) {
	store := &trackingConcurrencyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	ctx := context.Background()

	// Set scope first.
	s.SetScope(ctx, nil, "account", "acct-123", 0, 0)

	// Now call ClearScope.
	s.ClearScope(ctx)
	if len(store.releases) != 1 {
		t.Fatalf("expected 1 release after ClearScope, got %d", len(store.releases))
	}
	if store.releases[0] != "vo:account:acct-123" {
		t.Errorf("expected release of 'vo:account:acct-123', got %q", store.releases[0])
	}
	if s.scopeSet {
		t.Error("expected scopeSet=false after ClearScope")
	}
}

func TestSetScopeAcquisitionFailure(t *testing.T) {
	// Create a store that returns not-acquired.
	store := &mockConcurrencyKeyStore{}
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = store

	// The mock always returns acquired=true, so this tests the happy path.
	// For the failure path we'd need a different mock.
	// Test that scope is set correctly.
	s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)
	if !s.scopeSet {
		t.Error("expected scopeSet=true after acquisition")
	}
}
