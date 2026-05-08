package host

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/host/testutil"
)

// ---------------------------------------------------------------------------
// Mock concurrency key store for unit tests (no DB required)
// ---------------------------------------------------------------------------

// mockConcurrencyKeyStore provides an in-memory ConcurrencyKeyStore for tests.
// It tracks key ownership and supports configurable TTL expiry and error
// injection for timeout scenarios.
type mockConcurrencyKeyStore struct {
	mu    sync.Mutex
	keys  map[string]concurrencyKeyEntry // key -> entry
}

type concurrencyKeyEntry struct {
	workflowID string
	expiresAt  time.Time
}

func newMockConcurrencyKeyStore() *mockConcurrencyKeyStore {
	return &mockConcurrencyKeyStore{
		keys: make(map[string]concurrencyKeyEntry),
	}
}

func (m *mockConcurrencyKeyStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, exists := m.keys[key]; exists {
		// Check if the key has expired.
		if time.Now().After(entry.expiresAt) {
			delete(m.keys, key)
		} else if entry.workflowID != workflowID {
			// Key held by another workflow.
			return false, nil
		}
	}

	m.keys[key] = concurrencyKeyEntry{
		workflowID: workflowID,
		expiresAt:  time.Now().Add(ttl),
	}
	return true, nil
}

func (m *mockConcurrencyKeyStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
	return nil
}

// ---------------------------------------------------------------------------
// Concurrency key unit tests using execSession with mock store
// ---------------------------------------------------------------------------

// TestConcurrencyKeySameKeyBlocks verifies that two workflows using the same
// concurrency key (AcquireLock) cannot both acquire it simultaneously.
func TestConcurrencyKeySameKeyBlocks(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Session 1: acquires lock successfully.
	session1 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-session-1",
	}
	result1 := session1.freshAcquireLock(ctx, mod, "shared-key", 30000)
	acquired1 := ((result1 >> 8) & 0xFF) != 0
	errCode1 := byte(result1 & 0xFF)
	if errCode1 != 0 {
		t.Fatalf("session1 acquire: unexpected errCode=%d", errCode1)
	}
	if !acquired1 {
		t.Error("session1 should have acquired the lock")
	}

	// Session 2: tries same key — should fail to acquire.
	session2 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-session-2",
	}
	result2 := session2.freshAcquireLock(ctx, mod, "shared-key", 30000)
	acquired2 := ((result2 >> 8) & 0xFF) != 0
	errCode2 := byte(result2 & 0xFF)
	if errCode2 != 0 {
		t.Fatalf("session2 acquire: unexpected errCode=%d", errCode2)
	}
	if acquired2 {
		t.Error("session2 should NOT have acquired the lock (session1 holds it)")
	}

	// Verify session1 recorded an acquired event.
	if len(session1.history) < 1 || session1.history[0].LockAcquired != 1 {
		t.Error("session1 history should show lock acquired")
	}
	// Verify session2 recorded a non-acquired event.
	if len(session2.history) < 1 || session2.history[0].LockAcquired != 0 {
		t.Error("session2 history should show lock NOT acquired")
	}
}

// TestConcurrencyKeyRelease verifies that releasing a concurrency key allows
// another workflow to acquire it.
func TestConcurrencyKeyRelease(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Session A acquires the lock.
	sessionA := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-session-A",
	}
	resultA := sessionA.freshAcquireLock(ctx, mod, "release-key", 30000)
	acquiredA := ((resultA >> 8) & 0xFF) != 0
	if !acquiredA {
		t.Fatal("sessionA should acquire the lock")
	}

	// Session B tries — should be blocked.
	sessionB := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-session-B",
	}
	resultB := sessionB.freshAcquireLock(ctx, mod, "release-key", 30000)
	acquiredB := ((resultB >> 8) & 0xFF) != 0
	if acquiredB {
		t.Fatal("sessionB should be blocked before release")
	}

	// Session A releases the lock.
	releaseResult := sessionA.freshReleaseLock(ctx, mod, "release-key")
	if releaseResult != 0 {
		t.Errorf("expected successful release (0), got %d", releaseResult)
	}

	// Session B retries — should now succeed.
	resultB2 := sessionB.freshAcquireLock(ctx, mod, "release-key", 30000)
	acquiredB2 := ((resultB2 >> 8) & 0xFF) != 0
	errCodeB2 := byte(resultB2 & 0xFF)
	if errCodeB2 != 0 {
		t.Fatalf("sessionB retry: unexpected errCode=%d", errCodeB2)
	}
	if !acquiredB2 {
		t.Error("sessionB should acquire the lock after release")
	}
}

// TestConcurrencyKeyTimeout verifies that a lock acquisition failure is
// properly recorded when the concurrency key is held by another workflow,
// and that the error propagates through the event history.
func TestConcurrencyKeyTimeout(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Session1 acquires with a very short TTL.
	session1 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-t1",
	}
	_ = session1.freshAcquireLock(ctx, mod, "ttl-key", 1 /* 1ms TTL */)

	// Session2 tries immediately — key is still held.
	session2 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-t2",
	}
	result2 := session2.freshAcquireLock(ctx, mod, "ttl-key", 30000)
	acquired2 := ((result2 >> 8) & 0xFF) != 0
	if acquired2 {
		t.Error("session2 should NOT acquire while session1 holds the key")
	}
	if len(session2.history) < 1 {
		t.Fatal("expected at least one event in session2 history")
	}
	if session2.history[0].LockAcquired != 0 {
		t.Error("session2 history should record LockAcquired=0")
	}
}

// TestConcurrencyKeyDifferentKeys verifies that acquisition of different keys
// does not interfere.
func TestConcurrencyKeyDifferentKeys(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	sessionA := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-diff-A",
	}
	sessionB := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:   make([]EventRecord, 0),
		workflowID: "wf-diff-B",
	}

	// Both acquire different keys — should both succeed.
	resultA := sessionA.freshAcquireLock(ctx, mod, "key-a", 30000)
	resultB := sessionB.freshAcquireLock(ctx, mod, "key-b", 30000)

	acquiredA := ((resultA >> 8) & 0xFF) != 0
	acquiredB := ((resultB >> 8) & 0xFF) != 0

	if !acquiredA {
		t.Error("sessionA should acquire key-a")
	}
	if !acquiredB {
		t.Error("sessionB should acquire key-b")
	}
}

// ---------------------------------------------------------------------------
// Concurrency key tests against PostgresStore directly.
// These require a real PostgreSQL database. Set CLEAT_TEST_DB to run.
// Example: CLEAT_TEST_DB="postgres://localhost:5432/cleat?sslmode=disable" go test -v -run TestConcurrency
// ---------------------------------------------------------------------------

func TestConcurrencyKeyAcquireRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testutil.TestDB(t)
	defer db.Close()
	testutil.SetupFullSchema(t, db)

	store := NewPostgresStore(db)
	ctx := context.Background()
	key := fmt.Sprintf("test-key-%d", time.Now().UnixNano())
	wfID1 := fmt.Sprintf("wf-1-%d", time.Now().UnixNano())
	wfID2 := fmt.Sprintf("wf-2-%d", time.Now().UnixNano())

	// Create test workflow instances (needed for FK constraint if concurrency_keys has one).
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID1)
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID2)

	t.Cleanup(func() {
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID1)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID2)
	})

	// 1. First acquire should succeed.
	acquired, err := store.AcquireConcurrencyKey(ctx, key, wfID1, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to succeed")
	}

	// 2. Second acquire on same key with different workflow should fail (409 conflict).
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Fatal("expected second acquire to fail (key already held)")
	}

	// 3. Release the key.
	if err := store.ReleaseConcurrencyKey(ctx, key); err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}

	// 4. After release, re-acquire should succeed.
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after release: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire after release to succeed")
	}
}

func TestConcurrencyKeyExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testutil.TestDB(t)
	defer db.Close()
	testutil.SetupFullSchema(t, db)

	store := NewPostgresStore(db)
	ctx := context.Background()
	key := fmt.Sprintf("expiry-test-key-%d", time.Now().UnixNano())
	wfID1 := fmt.Sprintf("wf-exp-1-%d", time.Now().UnixNano())
	wfID2 := fmt.Sprintf("wf-exp-2-%d", time.Now().UnixNano())

	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID1)
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID2)

	t.Cleanup(func() {
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID1)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID2)
	})

	// Acquire with very short TTL (1 second).
	acquired, err := store.AcquireConcurrencyKey(ctx, key, wfID1, 1*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to succeed")
	}

	// Immediate re-acquire should fail (not expired yet).
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Fatal("expected immediate second acquire to fail before TTL expiry")
	}

	// Wait for TTL to expire.
	time.Sleep(1100 * time.Millisecond)

	// Now should be able to acquire (AcquireConcurrencyKey cleans up expired keys).
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after expiry: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire after TTL expiry to succeed")
	}
}

func TestConcurrencyKeyStartAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testutil.TestDB(t)
	defer db.Close()
	testutil.SetupFullSchema(t, db)

	store := NewPostgresStore(db)
	ctx := context.Background()
	key := fmt.Sprintf("api-test-key-%d", time.Now().UnixNano())
	wfID1 := fmt.Sprintf("wf-api-1-%d", time.Now().UnixNano())
	wfID2 := fmt.Sprintf("wf-api-2-%d", time.Now().UnixNano())

	// Create two test workflow instances.
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID1)
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID2)

	t.Cleanup(func() {
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID1)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID2)
	})

	// Simulate handleStartWorkflow: acquire key for first workflow.
	acquired, err := store.AcquireConcurrencyKey(ctx, key, wfID1, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to succeed")
	}

	// Second acquire should fail (like 409 conflict).
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Fatal("expected second acquire to fail (409 conflict)")
	}

	// Simulate handleStartWorkflow failure path: fail the second workflow on conflict.
	err = store.FailWorkflow(ctx, wfID2, "", "concurrency key conflict: "+key, nil)
	if err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	// Verify first workflow still holds its key (FailWorkflow only releases keys for the failed workflow).
	// wfID2 should not have acquired any key, so nothing to verify there.
	// wfID1 should still hold the key.
	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Fatal("expected key to still be held by wfID1 after wfID2 was failed")
	}

	// Release the key for wfID1 (simulating wfID1 completing).
	err = store.ReleaseWorkflowConcurrencyKeys(ctx, wfID1)
	if err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
	}

	// After release, a new workflow can acquire.
	wfID3 := fmt.Sprintf("wf-api-3-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID3)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID3)
	})

	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID3, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after release: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire after release to succeed")
	}
}

func TestConcurrencyKeyReleaseOnComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testutil.TestDB(t)
	defer db.Close()
	testutil.SetupFullSchema(t, db)

	store := NewPostgresStore(db)
	ctx := context.Background()
	key := fmt.Sprintf("complete-test-key-%d", time.Now().UnixNano())
	wfID := fmt.Sprintf("wf-comp-%d", time.Now().UnixNano())

	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID)
	})

	// Acquire key.
	acquired, err := store.AcquireConcurrencyKey(ctx, key, wfID, 30*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("AcquireConcurrencyKey: acquired=%v err=%v", acquired, err)
	}

	// CompleteWorkflow should release the key.
	err = store.CompleteWorkflow(ctx, wfID, "", `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// Now another workflow should be able to acquire.
	wfID2 := fmt.Sprintf("wf-comp-2-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID2)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID2)
	})

	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after CompleteWorkflow: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire to succeed after CompleteWorkflow released keys")
	}
}

func TestConcurrencyKeyReleaseOnFail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testutil.TestDB(t)
	defer db.Close()
	testutil.SetupFullSchema(t, db)

	store := NewPostgresStore(db)
	ctx := context.Background()
	key := fmt.Sprintf("fail-test-key-%d", time.Now().UnixNano())
	wfID := fmt.Sprintf("wf-fail-%d", time.Now().UnixNano())

	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID)
	})

	// Acquire key.
	acquired, err := store.AcquireConcurrencyKey(ctx, key, wfID, 30*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("AcquireConcurrencyKey: acquired=%v err=%v", acquired, err)
	}

	// FailWorkflow should release the key.
	err = store.FailWorkflow(ctx, wfID, "", "something went wrong", nil)
	if err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	// Now another workflow should be able to acquire.
	wfID2 := fmt.Sprintf("wf-fail-2-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, wfID2)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID2)
	})

	acquired, err = store.AcquireConcurrencyKey(ctx, key, wfID2, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after FailWorkflow: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire to succeed after FailWorkflow released keys")
	}
}
