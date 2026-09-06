package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock signal store for unit tests (no DB required)
// ---------------------------------------------------------------------------

// mockSignalWorkflowStore implements the signal-related methods of WorkflowStore
// in memory: DeliverSignal, PollSignal (non-consuming), ConsumeSignal, and
// PollCancellation.
//
// It was `map[string]string` keyed "workflowID:signalName" until
// IMPROVEMENT-PLAN 3.215, which is to say it reproduced the schema's defect
// exactly -- one signal per name, a second delivery overwriting the first --
// so no test using it could observe the bug. It is now a queue per
// (workflow, name), which is what the table became.
type mockSignalWorkflowStore struct {
	mu             sync.Mutex
	nextID         int64
	signals        map[string][]SignalDelivery // key = "workflowID:signalName" -> queue, oldest first
	pollCount      int                         // total PollSignal calls
	consumeCount   int                         // total ConsumeSignal calls
	deliverCount   int                         // total DeliverSignal calls
	allowedCallers []string                    // for GetAllowedSignalCallers
}

func newMockSignalWorkflowStore() *mockSignalWorkflowStore {
	return &mockSignalWorkflowStore{
		signals: make(map[string][]SignalDelivery),
	}
}

func (m *mockSignalWorkflowStore) signalKey(workflowID, signalName string) string {
	return workflowID + ":" + signalName
}

// pollAndConsume is what the await path does: read the head, then remove it.
// It exists so these tests exercise the two calls in the order the engine makes
// them -- see execSession.consumeDelivered for why that order is load-bearing.
func (m *mockSignalWorkflowStore) pollAndConsume(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	d, found, err := m.PollSignal(ctx, workflowID, signalName)
	if err != nil || !found {
		return "", found, err
	}
	return d.Payload, true, m.ConsumeSignal(ctx, workflowID, d.ID)
}

// DeliverSignal appends a delivery. Two signals of the same name are two
// deliveries; neither replaces the other.
func (m *mockSignalWorkflowStore) DeliverSignal(_ context.Context, workflowID, signalName, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliverCount++
	m.nextID++
	key := m.signalKey(workflowID, signalName)
	m.signals[key] = append(m.signals[key], SignalDelivery{ID: m.nextID, Payload: payload})
	return nil
}

// PollSignal returns the oldest delivery with this name without consuming it.
func (m *mockSignalWorkflowStore) PollSignal(_ context.Context, workflowID, signalName string) (SignalDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollCount++
	q := m.signals[m.signalKey(workflowID, signalName)]
	if len(q) == 0 {
		return SignalDelivery{}, false, nil
	}
	return q[0], true, nil
}

// ConsumeSignal removes one delivery by id. An id that is already gone is not
// an error, matching the real stores.
func (m *mockSignalWorkflowStore) ConsumeSignal(_ context.Context, workflowID string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumeCount++
	for key, q := range m.signals {
		if !strings.HasPrefix(key, workflowID+":") {
			continue
		}
		for i, d := range q {
			if d.ID == id {
				m.signals[key] = append(q[:i], q[i+1:]...)
				return nil
			}
		}
	}
	return nil
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
	d, found, err := store.PollSignal(ctx, "wf-001", "payment_confirmed")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found after delivery")
	}
	if d.Payload != `{"txn_id":"txn-001","amount":5000}` {
		t.Errorf("expected payload %q, got %q", `{"txn_id":"txn-001","amount":5000}`, d.Payload)
	}
	if store.pollCount != 1 {
		t.Errorf("expected pollCount=1, got %d", store.pollCount)
	}
}

// TestPollThenConsumeRemovesTheDelivery verifies that the await path's two
// calls -- poll the head, then consume it by id -- make the delivery
// unavailable to the next poll.
//
// This replaces TestPollAndClaimSignalConsumed, which tested a method
// (PollAndClaimSignal) that had no caller anywhere in the engine. The
// behaviour was correct and unreachable, which is IMPROVEMENT-PLAN 3.215's
// (d): signals were read and never consumed on the live path, and the only
// method that would have consumed one was dead. A test over a dead method is
// how that survived.
func TestPollThenConsumeRemovesTheDelivery(t *testing.T) {
	ctx := context.Background()
	store := newMockSignalWorkflowStore()

	// Deliver a signal.
	err := store.DeliverSignal(ctx, "wf-002", "order_shipped", `{"order_id":"ord-123","tracking":"TRACK-001"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// First poll-and-consume should return the signal.
	payload, found, err := store.pollAndConsume(ctx, "wf-002", "order_shipped")
	if err != nil {
		t.Fatalf("pollAndConsume: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found on first poll")
	}
	if payload != `{"order_id":"ord-123","tracking":"TRACK-001"}` {
		t.Errorf("expected payload %q, got %q", `{"order_id":"ord-123","tracking":"TRACK-001"}`, payload)
	}
	if store.consumeCount != 1 {
		t.Errorf("expected consumeCount=1, got %d", store.consumeCount)
	}

	// Second should return not found: the delivery is gone.
	_, found, err = store.pollAndConsume(ctx, "wf-002", "order_shipped")
	if err != nil {
		t.Fatalf("pollAndConsume second call: %v", err)
	}
	if found {
		t.Fatal("expected the delivery to be gone after it was consumed")
	}
	if store.consumeCount != 1 {
		t.Errorf("a poll that found nothing must not consume: expected consumeCount=1, got %d", store.consumeCount)
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
		d, found, err := store.PollSignal(ctx, "wf-003", "approval_granted")
		if err != nil {
			t.Fatalf("PollSignal iteration %d: %v", i, err)
		}
		if !found {
			t.Fatalf("iteration %d: expected signal to still be found", i)
		}
		if d.Payload != `{"approved":true,"role":"admin"}` {
			t.Errorf("iteration %d: expected payload %q, got %q",
				i, `{"approved":true,"role":"admin"}`, d.Payload)
		}
	}

	// PollSignal count should be 3.
	if store.pollCount != 3 {
		t.Errorf("expected pollCount=3, got %d", store.pollCount)
	}

	// Verify the delivery is still consumable after the non-destructive polls.
	payload, found, err := store.pollAndConsume(ctx, "wf-003", "approval_granted")
	if err != nil {
		t.Fatalf("pollAndConsume after poll: %v", err)
	}
	if !found {
		t.Fatal("expected delivery to still be consumable after non-destructive polls")
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
	payload, found, err := store.pollAndConsume(ctx, "", "test_signal")
	if err != nil {
		t.Fatalf("pollAndConsume: %v", err)
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
	payload, found, err = store.pollAndConsume(ctx, "wf-004", "")
	if err != nil {
		t.Fatalf("pollAndConsume empty name: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be found with empty signal name")
	}
	if payload != `{"data":"test2"}` {
		t.Errorf("expected payload %q, got %q", `{"data":"test2"}`, payload)
	}
}

// TestSignalDeliveredTwiceArrivesTwiceOldestFirst verifies that delivering the
// same (workflowID, signalName) pair twice produces TWO deliveries, and that
// the first one to arrive is the first one read.
//
// This test is the inversion of TestSignalDeliveryTwiceOverwrites, which
// asserted the opposite -- "only the latest version should be present" -- as
// intended behaviour. It was not: the overwrite was a consequence of
// PRIMARY KEY (workflow_id, signal_name), and it silently discarded the first
// payload on the ordinary path (IMPROVEMENT-PLAN 3.215(b)). A workflow
// collecting one approval per reviewer saw only the last reviewer.
//
// A test asserting a defect is worse than no test, because it converts the
// defect into a requirement and makes fixing it look like a regression. This
// is the fourth of that shape found in this repo.
func TestSignalDeliveredTwiceArrivesTwiceOldestFirst(t *testing.T) {
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

	// Both are present, oldest first.
	payload, found, err := store.pollAndConsume(ctx, "wf-005", "status_update")
	if err != nil {
		t.Fatalf("first pollAndConsume: %v", err)
	}
	if !found {
		t.Fatal("expected the first delivery to be found")
	}
	if payload != `{"status":"pending"}` {
		t.Errorf("expected the FIRST payload %q, got %q", `{"status":"pending"}`, payload)
	}

	payload, found, err = store.pollAndConsume(ctx, "wf-005", "status_update")
	if err != nil {
		t.Fatalf("second pollAndConsume: %v", err)
	}
	if !found {
		t.Fatal("expected the second delivery to be found -- it was discarded by the overwrite this test used to assert")
	}
	if payload != `{"status":"completed","result":"ok"}` {
		t.Errorf("expected the SECOND payload %q, got %q",
			`{"status":"completed","result":"ok"}`, payload)
	}

	// And nothing else.
	if _, found, err = store.pollAndConsume(ctx, "wf-005", "status_update"); err != nil {
		t.Fatalf("third pollAndConsume: %v", err)
	} else if found {
		t.Fatal("expected the queue to be empty after both deliveries were consumed")
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
	payload, found, err := store.pollAndConsume(ctx, "non-existent-workflow-id", "test_signal")
	if err != nil {
		t.Fatalf("pollAndConsume after delivery: %v", err)
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
	payload, found, err = store.pollAndConsume(ctx, "non-existent-workflow-id", "another_signal")
	if err != nil {
		t.Fatalf("pollAndConsume second signal: %v", err)
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
	d, found, err := store.PollSignal(ctx, "wf-never", "never_delivered")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if found {
		t.Fatal("expected found=false for never-delivered signal")
	}
	if d.Payload != "" {
		t.Errorf("expected empty payload, got %q", d.Payload)
	}
	if d.ID != 0 {
		t.Errorf("expected zero id alongside found=false, got %d", d.ID)
	}
	if store.pollCount != 1 {
		t.Errorf("expected pollCount=1, got %d", store.pollCount)
	}

	// Verify the await path's poll-then-consume also returns not found, and
	// consumes nothing.
	_, found, err = store.pollAndConsume(ctx, "wf-never", "never_delivered")
	if err != nil {
		t.Fatalf("pollAndConsume: %v", err)
	}
	if found {
		t.Fatal("expected pollAndConsume to return not-found for never-delivered signal")
	}
	if store.consumeCount != 0 {
		t.Errorf("expected consumeCount=0 when nothing was found, got %d", store.consumeCount)
	}

	// Verify that after delivering a signal, PollSignal returns it.
	err = store.DeliverSignal(ctx, "wf-never", "now_delivered", `{"status":"ok"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	d, found, err = store.PollSignal(ctx, "wf-never", "now_delivered")
	if err != nil {
		t.Fatalf("PollSignal after delivery: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after delivery")
	}
	if d.Payload != `{"status":"ok"}` {
		t.Errorf("expected payload %q, got %q", `{"status":"ok"}`, d.Payload)
	}
	if d.ID == 0 {
		t.Error("a found delivery must carry a non-zero id: ConsumeSignal has nothing else to address")
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

func (_ *mockSignalWorkflowStore) SetAllowedSignalCallers(_ context.Context, _ string, _ []string) error {
	return nil
}
