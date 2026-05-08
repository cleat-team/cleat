package host

import (
	"context"
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
	mu           sync.Mutex
	signals      map[string]string // key = "workflowID:signalName" -> payload
	pollCount    int               // total PollSignal calls
	claimCount   int               // total PollAndClaimSignal calls
	deliverCount int               // total DeliverSignal calls
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
