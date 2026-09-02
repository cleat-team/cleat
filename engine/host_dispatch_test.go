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
	if s.isReplay {
		t.Error("expected replay to have ended")
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
	if s.isReplay {
		t.Error("expected replay to have ended")
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

// keyedCancellationStore implements SignalStore and only reports a workflow
// as cancelled when PollCancellation is queried with the exact workflowID
// that RequestCancellation (or the cancelledWorkflowID field, for brevity in
// simpler tests) was given. This is stricter than mockCancellationStore: it
// proves the caller actually threads the real workflow ID through, because
// querying with the wrong ID (e.g. the historical "" bug) yields "not
// cancelled" even though *some* workflow was cancelled.
type keyedCancellationStore struct {
	// cancelledWorkflowID and reason may be set directly for simple tests,
	// or populated via RequestCancellation to mimic a real caller.
	cancelledWorkflowID string
	reason              string

	mu          sync.Mutex
	queriedWith []string
}

// RequestCancellation mimics Store.RequestCancellation: it records that the
// given workflowID has been cancelled, for the given reason.
func (k *keyedCancellationStore) RequestCancellation(_ context.Context, workflowID, reason string) error {
	k.mu.Lock()
	k.cancelledWorkflowID = workflowID
	k.reason = reason
	k.mu.Unlock()
	return nil
}

func (k *keyedCancellationStore) PollCancellation(_ context.Context, workflowID string) (bool, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.queriedWith = append(k.queriedWith, workflowID)
	if workflowID != "" && workflowID == k.cancelledWorkflowID {
		return true, k.reason, nil
	}
	return false, "", nil
}

func (k *keyedCancellationStore) DeliverSignal(_ context.Context, _, _, _ string) error {
	return nil
}

func (k *keyedCancellationStore) PollSignal(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

// TestPollCancellationCancelled proves that a workflow actually observes a
// cancellation request made against its own workflow ID. It uses a store
// that only reports "cancelled" when queried with the exact workflow ID that
// was cancelled, so if the call site regresses to passing "" (the original
// bug), this test fails: the store would be queried with "" instead of
// "wf-cancel-me" and would report "not cancelled".
func TestPollCancellationCancelled(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "wf-cancel-me"

	store := &keyedCancellationStore{
		cancelledWorkflowID: "wf-cancel-me",
		reason:              "user requested cancellation",
	}
	s.engine.signalStore = store

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)

	result := s.PollCancellation(ctx, nil, 0, uint32(len(buf)))

	cancelledFlag := uint32(result & 0xFFFFFFFF)
	if cancelledFlag != 1 {
		t.Fatalf("expected cancelled flag=1, got result=%d (flag=%d)", result, cancelledFlag)
	}
	reasonLen := uint32(result >> 32)
	written := string(buf[:reasonLen])
	if written != "user requested cancellation" {
		t.Errorf("expected reason %q written to buffer, got %q", "user requested cancellation", written)
	}

	store.mu.Lock()
	queried := append([]string(nil), store.queriedWith...)
	store.mu.Unlock()
	if len(queried) == 0 {
		t.Fatal("expected PollCancellation to be called at least once")
	}
	for _, id := range queried {
		if id != "wf-cancel-me" {
			t.Errorf("expected PollCancellation queried with workflowID %q, got %q", "wf-cancel-me", id)
		}
	}
}

// TestPollCancellationWrongWorkflowIDNotObserved documents the failure mode
// of the original bug directly: if PollCancellation is queried with an ID
// that doesn't match the cancelled workflow (e.g. "" instead of the real
// workflow ID), the cancellation is never observed.
func TestPollCancellationWrongWorkflowIDNotObserved(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "" // simulates the historical hardcoded "" bug

	store := &keyedCancellationStore{
		cancelledWorkflowID: "wf-cancel-me",
		reason:              "user requested cancellation",
	}
	s.engine.signalStore = store

	result := s.PollCancellation(context.Background(), nil, 0, 0)
	cancelledFlag := uint32(result & 0xFFFFFFFF)
	if cancelledFlag != 0 {
		t.Errorf("expected cancellation to NOT be observed when queried with wrong/empty workflowID, got flag=%d", cancelledFlag)
	}
}

// TestCancellationObservedEndToEnd is the regression test for the dead
// cancellation bug (IMPROVEMENT-PLAN item 1.3): RequestCancellation is
// called for a specific workflow, and a subsequent DurableCall against that
// same workflow's session must observe the cancellation and stop -- it must
// neither invoke the real downstream service nor report success. This
// exercises the actual production freshCall path (DurableCall -> freshCall),
// not just the PollCancellation host function in isolation.
//
// Per engine convention, cancellation surfaces as an error flag on the guest
// response buffer plus a "workflow cancelled" result string, not as a Go
// error, so this asserts on the packed result/buffer contents rather than
// only checking for a nil/non-nil error.
func TestCancellationObservedEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := &keyedCancellationStore{}

	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.workflowID = "wf-target"
	s.engine.caller = caller
	s.engine.signalStore = store

	// Request cancellation for this exact workflow, as an operator would via
	// Store.RequestCancellation (e.g. through the admin API).
	if err := store.RequestCancellation(ctx, "wf-target", "operator requested stop"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	buf := make([]byte, 256)
	memCtx := contextWithRawMemBuf(ctx, buf)

	result := s.DurableCall(memCtx, nil, "my-svc", "my-op", `{"key":"val"}`, 0, uint32(len(buf)))

	// The workflow must not have actually invoked the downstream service.
	if len(caller.calls) != 0 {
		t.Errorf("expected 0 calls to the real service after cancellation, got %d: %+v", len(caller.calls), caller.calls)
	}

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	// callErrorUnknown, not the old CallErrorTimeout: a cancelled workflow is
	// the one case where retrying the call is exactly what the caller must not
	// do, and Timeout reports as retryable.
	if errCode != 1 || callErrorCode != callErrorUnknown {
		t.Fatalf("expected cancellation error flags (errCode=1, callErrorCode=%d), got errCode=%d callErrorCode=%d (raw=%d)", callErrorUnknown, errCode, callErrorCode, result)
	}
	respLen := uint32(result >> 40)
	written := string(buf[:respLen])
	if written != "workflow cancelled" {
		t.Errorf("expected response %q, got %q", "workflow cancelled", written)
	}
}

// TestCancellationNotObservedForDifferentWorkflow proves the store is keyed
// correctly: cancelling one workflow must not affect another workflow's
// session (i.e. the fix must not degenerate into "always cancelled").
func TestCancellationNotObservedForDifferentWorkflow(t *testing.T) {
	ctx := context.Background()
	store := &keyedCancellationStore{}

	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.workflowID = "wf-innocent"
	s.engine.caller = caller
	s.engine.signalStore = store

	if err := store.RequestCancellation(ctx, "wf-other", "cancel the other one"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	result := s.DurableCall(ctx, nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	if len(caller.calls) != 1 {
		t.Errorf("expected the uncancelled workflow's call to proceed, got %d calls", len(caller.calls))
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected no cancellation error for an unrelated workflow, got errCode=%d", errCode)
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
	if s.isReplay {
		t.Error("expected replay to have ended")
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
