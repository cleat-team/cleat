package host

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock signal store for unit tests (no DB required)
// ---------------------------------------------------------------------------

// mockSignalWorkflowStore implements the signal-related methods of WorkflowStore
// using an in-memory map. It supports DeliverSignal, PollSignal (non-destructive),
// PollAndClaimSignal (destructive), and PollCancellation.
type mockSignalWorkflowStore struct {
	mu             sync.Mutex
	signals        map[string]string // key = "workflowID:signalName" -> payload
	pollCount      int               // total PollSignal calls
	claimCount     int               // total PollAndClaimSignal calls
	deliverCount   int               // total DeliverSignal calls
	allowedCallers []string          // for GetAllowedSignalCallers
}

func newMockSignalWorkflowStore() *mockSignalWorkflowStore {
	return &mockSignalWorkflowStore{
		signals: make(map[string]string),
	}
}

func (m *mockSignalWorkflowStore) signalKey(workflowID, signalName string) string {
	return workflowID + ":" + signalName
}

// DeliverSignal stores a signal in memory. If the same (workflowID, signalName)
// pair already exists, it is overwritten with the new payload.
func (m *mockSignalWorkflowStore) DeliverSignal(_ context.Context, workflowID, signalName, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliverCount++
	m.signals[m.signalKey(workflowID, signalName)] = payload
	return nil
}

// PollSignal checks for a signal without consuming it (non-destructive).
// Returns the payload if the signal exists.
func (m *mockSignalWorkflowStore) PollSignal(_ context.Context, workflowID, signalName string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollCount++
	payload, ok := m.signals[m.signalKey(workflowID, signalName)]
	if !ok {
		return "", false, nil
	}
	return payload, true, nil
}

// PollAndClaimSignal checks for a signal and removes it atomically (destructive).
// Returns the payload if the signal existed.
func (m *mockSignalWorkflowStore) PollAndClaimSignal(_ context.Context, workflowID, signalName string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimCount++
	key := m.signalKey(workflowID, signalName)
	payload, ok := m.signals[key]
	if !ok {
		return "", false, nil
	}
	delete(m.signals, key)
	return payload, true, nil
}

// PollCancellation always returns not cancelled.
func (m *mockSignalWorkflowStore) PollCancellation(_ context.Context, _ string) (bool, string, error) {
	return false, "", nil
}

// GetAllowedSignalCallers returns the pre-configured allowed callers list.
func (m *mockSignalWorkflowStore) GetAllowedSignalCallers(_ context.Context, _ string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowedCallers, nil
}


// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestDeliverSignalViaMockStore verifies that a signal delivered to the mock
// store can be retrieved via PollSignal, and the delivery count increments.
func TestDeliverSignalViaMockStore(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver a signal.
	err := store.DeliverSignal(ctx, "wf-001", "payment_confirmed", `{"txn_id":"txn-001","amount":5000}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
	if store.deliverCount != 1 {
		t.Errorf("expected deliverCount=1, got %d", store.deliverCount)
	}

	// Verify the signal is stored by polling it.
	payload, found, err := store.PollSignal(ctx, "wf-001", "payment_confirmed")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found after delivery")
	}
	if payload != `{"txn_id":"txn-001","amount":5000}` {
		t.Errorf("expected payload %q, got %q", `{"txn_id":"txn-001","amount":5000}`, payload)
	}
	if store.pollCount != 1 {
		t.Errorf("expected pollCount=1, got %d", store.pollCount)
	}
}

// TestPollAndClaimSignalConsumed verifies that PollAndClaimSignal returns the
// signal payload on first call and returns not-found on second call (signal is
// consumed atomically).
func TestPollAndClaimSignalConsumed(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver a signal.
	err := store.DeliverSignal(ctx, "wf-002", "order_shipped", `{"order_id":"ord-123","tracking":"TRACK-001"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// First PollAndClaimSignal should return the signal.
	payload, found, err := store.PollAndClaimSignal(ctx, "wf-002", "order_shipped")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found on first claim")
	}
	if payload != `{"order_id":"ord-123","tracking":"TRACK-001"}` {
		t.Errorf("expected payload %q, got %q", `{"order_id":"ord-123","tracking":"TRACK-001"}`, payload)
	}
	if store.claimCount != 1 {
		t.Errorf("expected claimCount=1, got %d", store.claimCount)
	}

	// Second PollAndClaimSignal should return not found (signal consumed).
	_, found, err = store.PollAndClaimSignal(ctx, "wf-002", "order_shipped")
	if err != nil {
		t.Fatalf("PollAndClaimSignal second call: %v", err)
	}
	if found {
		t.Fatal("expected signal to be consumed after first claim")
	}
	if store.claimCount != 2 {
		t.Errorf("expected claimCount=2, got %d", store.claimCount)
	}
}

// TestPollSignalNonDestructive verifies that PollSignal does NOT consume the
// signal — calling it multiple times returns the same payload.
func TestPollSignalNonDestructive(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver a signal.
	err := store.DeliverSignal(ctx, "wf-003", "approval_granted", `{"approved":true,"role":"admin"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// Poll the same signal multiple times — it should still be found each time.
	for i := 0; i < 3; i++ {
		payload, found, err := store.PollSignal(ctx, "wf-003", "approval_granted")
		if err != nil {
			t.Fatalf("PollSignal iteration %d: %v", i, err)
		}
		if !found {
			t.Fatalf("iteration %d: expected signal to still be found", i)
		}
		if payload != `{"approved":true,"role":"admin"}` {
			t.Errorf("iteration %d: expected payload %q, got %q",
				i, `{"approved":true,"role":"admin"}`, payload)
		}
	}

	// PollSignal count should be 3.
	if store.pollCount != 3 {
		t.Errorf("expected pollCount=3, got %d", store.pollCount)
	}

	// Verify signal also still exists via PollAndClaimSignal.
	payload, found, err := store.PollAndClaimSignal(ctx, "wf-003", "approval_granted")
	if err != nil {
		t.Fatalf("PollAndClaimSignal after poll: %v", err)
	}
	if !found {
		t.Fatal("expected signal to still be claimable after non-destructive polls")
	}
	if payload != `{"approved":true,"role":"admin"}` {
		t.Errorf("expected payload %q, got %q", `{"approved":true,"role":"admin"}`, payload)
	}
}

// TestSignalDeliveryInvalidWorkflowID verifies that delivering a signal with an
// empty workflow ID or empty signal name is handled without error by the store.
func TestSignalDeliveryInvalidWorkflowID(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver signal with empty workflow ID — should not panic or error.
	err := store.DeliverSignal(ctx, "", "test_signal", `{"data":"test"}`)
	if err != nil {
		t.Fatalf("DeliverSignal with empty workflow ID: %v", err)
	}

	// Even though the workflow ID is empty, the signal should be stored.
	payload, found, err := store.PollAndClaimSignal(ctx, "", "test_signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found even with empty workflow ID")
	}
	if payload != `{"data":"test"}` {
		t.Errorf("expected payload %q, got %q", `{"data":"test"}`, payload)
	}

	// Deliver signal with empty signal name — should not panic or error.
	err = store.DeliverSignal(ctx, "wf-004", "", `{"data":"test2"}`)
	if err != nil {
		t.Fatalf("DeliverSignal with empty signal name: %v", err)
	}

	// Verify it was stored under the empty signal name.
	payload, found, err = store.PollAndClaimSignal(ctx, "wf-004", "")
	if err != nil {
		t.Fatalf("PollAndClaimSignal empty name: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found with empty signal name")
	}
	if payload != `{"data":"test2"}` {
		t.Errorf("expected payload %q, got %q", `{"data":"test2"}`, payload)
	}
}

// TestSignalDeliveryTwiceOverwrites verifies that delivering the same
// (workflowID, signalName) pair twice overwrites the payload with the new value.
func TestSignalDeliveryTwiceOverwrites(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver first version of the signal.
	err := store.DeliverSignal(ctx, "wf-005", "status_update", `{"status":"pending"}`)
	if err != nil {
		t.Fatalf("first DeliverSignal: %v", err)
	}

	// Deliver an updated version.
	err = store.DeliverSignal(ctx, "wf-005", "status_update", `{"status":"completed","result":"ok"}`)
	if err != nil {
		t.Fatalf("second DeliverSignal: %v", err)
	}

	// Only the latest version should be present.
	payload, found, err := store.PollAndClaimSignal(ctx, "wf-005", "status_update")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found")
	}
	if payload != `{"status":"completed","result":"ok"}` {
		t.Errorf("expected latest payload %q, got %q",
			`{"status":"completed","result":"ok"}`, payload)
	}
	if store.deliverCount != 2 {
		t.Errorf("expected deliverCount=2, got %d", store.deliverCount)
	}
}

// ---------------------------------------------------------------------------
// Signal delivery to non-existent workflow (no-op at store level)
// ---------------------------------------------------------------------------

// TestSignalDeliveryToNonExistentWorkflow verifies that delivering a signal to
// a workflow that does not exist is handled as a no-op by the mock signal
// store. The signal is stored but no error is returned (the store has no
// referential integrity check — that is the caller's responsibility).
func TestSignalDeliveryToNonExistentWorkflow(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver a signal to a workflow that has never been registered.
	// The mock store should accept it without error (no referential check).
	err := store.DeliverSignal(ctx, "non-existent-workflow-id", "test_signal", `{"data":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal to non-existent workflow: %v", err)
	}
	if store.deliverCount != 1 {
		t.Errorf("expected deliverCount=1, got %d", store.deliverCount)
	}

	// Verify the signal was stored and can be polled.
	payload, found, err := store.PollAndClaimSignal(ctx, "non-existent-workflow-id", "test_signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal after delivery: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found even for non-existent workflow")
	}
	if payload != `{"data":"hello"}` {
		t.Errorf("expected payload %q, got %q", `{"data":"hello"}`, payload)
	}

	// Delivering another signal to the same non-existent workflow should also
	// succeed (the store is purely a signal queue with no workflow validation).
	err = store.DeliverSignal(ctx, "non-existent-workflow-id", "another_signal", `{"count":2}`)
	if err != nil {
		t.Fatalf("second DeliverSignal to non-existent workflow: %v", err)
	}
	if store.deliverCount != 2 {
		t.Errorf("expected deliverCount=2, got %d", store.deliverCount)
	}

	// Both signals should be independently pollable.
	payload, found, err = store.PollAndClaimSignal(ctx, "non-existent-workflow-id", "another_signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal second signal: %v", err)
	}
	if !found {
		t.Fatal("expected second signal to be found")
	}
	if payload != `{"count":2}` {
		t.Errorf("expected payload %q, got %q", `{"count":2}`, payload)
	}
}

// ---------------------------------------------------------------------------
// PollSignal for never-delivered signal
// ---------------------------------------------------------------------------

// TestPollSignalForNeverDeliveredSignal verifies that PollSignal returns
// not-found (found=false) when polling for a signal that was never delivered
// to the workflow, without returning an error.
func TestPollSignalForNeverDeliveredSignal(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Poll for a signal that was never delivered — should return not found.
	payload, found, err := store.PollSignal(ctx, "wf-never", "never_delivered")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if found {
		t.Fatal("expected found=false for never-delivered signal")
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}
	if store.pollCount != 1 {
		t.Errorf("expected pollCount=1, got %d", store.pollCount)
	}

	// Verify PollAndClaimSignal also returns not found.
	_, found, err = store.PollAndClaimSignal(ctx, "wf-never", "never_delivered")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if found {
		t.Fatal("expected PollAndClaimSignal to return not-found for never-delivered signal")
	}
	if store.claimCount != 1 {
		t.Errorf("expected claimCount=1, got %d", store.claimCount)
	}

	// Verify that after delivering a signal, PollSignal returns it.
	err = store.DeliverSignal(ctx, "wf-never", "now_delivered", `{"status":"ok"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	payload, found, err = store.PollSignal(ctx, "wf-never", "now_delivered")
	if err != nil {
		t.Fatalf("PollSignal after delivery: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after delivery")
	}
	if payload != `{"status":"ok"}` {
		t.Errorf("expected payload %q, got %q", `{"status":"ok"}`, payload)
	}

	// Polling for a completely different signal name should still return not found.
	_, found, err = store.PollSignal(ctx, "wf-never", "other_signal")
	if err != nil {
		t.Fatalf("PollSignal other signal: %v", err)
	}
	if found {
		t.Fatal("expected found=false for other never-delivered signal")
	}
}

// ---------------------------------------------------------------------------
// Signal auth tests
// ---------------------------------------------------------------------------

// TestSignalAuthAllowsCallerInList verifies the auth check closure allows
// a caller whose defName is in the allowed_signals list.
func TestSignalAuthAllowsCallerInList(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"payment-service", "order-service"}

	check := makeSignalAuthCheck(store)
	err := check(context.Background(), "target-wf", "payment-service")
	if err != nil {
		t.Fatalf("expected caller to be allowed, got: %v", err)
	}
}

// TestSignalAuthDeniesCallerNotInList verifies the auth check closure denies
// a caller whose defName is not in the allowed_signals list.
func TestSignalAuthDeniesCallerNotInList(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"payment-service", "order-service"}

	check := makeSignalAuthCheck(store)
	err := check(context.Background(), "target-wf", "fraud-service")
	if err == nil {
		t.Fatal("expected caller to be denied")
	}
}

// TestSignalAuthDeniesWhenAllowedCallersEmpty verifies the auth check closure
// denies all callers when allowed_signals is empty (fail-secure).
func TestSignalAuthDeniesWhenAllowedCallersEmpty(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = nil // empty

	check := makeSignalAuthCheck(store)
	err := check(context.Background(), "target-wf", "any-service")
	if err == nil {
		t.Fatal("expected caller to be denied when allowed_signals is empty")
	}
}

// ---------------------------------------------------------------------------
// F56 plugin call duration metric
// ---------------------------------------------------------------------------

// TestPluginCallDurationMetric verifies the cleat_plugin_call_duration_seconds
// histogram is registered and observable.
func TestPluginCallDurationMetric(t *testing.T) {
	if pluginCallDuration == nil {
		t.Fatal("pluginCallDuration histogram is nil — not registered")
	}
	// Observe a value.
	pluginCallDuration.WithLabelValues("test-plugin", "test-func").Observe(0.5)
}

// makeSignalAuthCheck returns a signalAuthCheck function backed by the store.
// This mirrors the closure wired in cmd/cleat-worker/main.go.
func makeSignalAuthCheck(store *mockSignalWorkflowStore) func(ctx context.Context, targetWorkflowID, callerDefName string) error {
	return func(ctx context.Context, targetWorkflowID, callerDefName string) error {
		callers, err := store.GetAllowedSignalCallers(ctx, targetWorkflowID)
		if err != nil {
			return err
		}
		if len(callers) == 0 {
			return fmt.Errorf("signal auth denied: workflow %s has no allowed callers configured", targetWorkflowID)
		}
		for _, c := range callers {
			if c == "*" || c == callerDefName {
				return nil
			}
		}
		return fmt.Errorf("signal auth denied: %s not in allowed_signals of %s", callerDefName, targetWorkflowID)
	}
}
// ---------------------------------------------------------------------------
// Wildcard signal auth tests
// ---------------------------------------------------------------------------

// TestSignalAuthAllowsWildcard verifies that "*" in allowed_signals permits
// any caller, regardless of their defName.
func TestSignalAuthAllowsWildcard(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"*"}

	check := makeSignalAuthCheck(store)
	err := check(context.Background(), "target-wf", "any-service")
	if err != nil {
		t.Fatalf("expected wildcard to allow any caller, got: %v", err)
	}
}

// TestSignalAuthWildcardWithOtherCallers verifies that "*" in a mixed
// allowed_signals list still permits any caller.
func TestSignalAuthWildcardWithOtherCallers(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"payment-service", "*", "order-service"}

	check := makeSignalAuthCheck(store)
	err := check(context.Background(), "target-wf", "unknown-service")
	if err != nil {
		t.Fatalf("expected wildcard to allow any caller even in mixed list, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SendSignalAndWait auth tests
// ---------------------------------------------------------------------------

// TestSendSignalAndWaitAuthDenied verifies that SendSignalAndWait returns an
// auth error code when the caller is not in the target's allowed_signals.
func TestSendSignalAndWaitAuthDenied(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"payment-service"}

	e := &Engine{
		requireSignalAuth: true,
		signalAuthCheck:   makeSignalAuthCheck(store),
		signalStore:       store,
	}
	s := &execSession{
		engine:  e,
		defName: "fraud-service",
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf", "test-signal", `{"key":"value"}`, 10000, 0, 0)

	if result != errSignalAuthRequiredInt {
		t.Fatalf("expected errSignalAuthRequiredInt (%d), got %d", errSignalAuthRequiredInt, result)
	}
}

// TestSendSignalAndWaitAuthAllowed verifies that SendSignalAndWait proceeds
// normally when the caller is in the target's allowed_signals.
func TestSendSignalAndWaitAuthAllowed(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"payment-service", "order-service"}

	e := &Engine{
		requireSignalAuth: true,
		signalAuthCheck:   makeSignalAuthCheck(store),
		signalStore:       store,
	}
	s := &execSession{
		engine:  e,
		defName: "payment-service",
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf", "test-signal", `{"key":"value"}`, 10000, 0, 0)

	if result == errSignalAuthRequiredInt {
		t.Fatal("expected auth to pass, but got errSignalAuthRequiredInt")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAwaitSignals {
		t.Fatalf("expected AwaitSignals event, got %v", s.history[0].EventType)
	}
}

// TestSendSignalAndWaitAuthDisabled verifies that SendSignalAndWait proceeds
// without auth check when requireSignalAuth is false.
func TestSendSignalAndWaitAuthDisabled(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{}

	e := &Engine{
		requireSignalAuth: false,
		signalAuthCheck:   makeSignalAuthCheck(store),
		signalStore:       store,
	}
	s := &execSession{
		engine:  e,
		defName: "any-service",
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf", "test-signal", `{"key":"value"}`, 10000, 0, 0)

	if result == errSignalAuthRequiredInt {
		t.Fatal("expected auth to be skipped when disabled, but got errSignalAuthRequiredInt")
	}
}

// TestSendSignalAndWaitAuthWithWildcard verifies that SendSignalAndWait
// succeeds when the target's allowed_signals includes "*".
func TestSendSignalAndWaitAuthWithWildcard(t *testing.T) {
	store := newMockSignalWorkflowStore()
	store.allowedCallers = []string{"*"}

	e := &Engine{
		requireSignalAuth: true,
		signalAuthCheck:   makeSignalAuthCheck(store),
		signalStore:       store,
	}
	s := &execSession{
		engine:  e,
		defName: "any-service",
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf", "test-signal", `{"key":"value"}`, 10000, 0, 0)

	if result == errSignalAuthRequiredInt {
		t.Fatal("expected wildcard to allow any caller, but got errSignalAuthRequiredInt")
	}
}
