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
