package host

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"

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

func (m *mockConcurrencyKeyStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, entry := range m.keys {
		if entry.workflowID == workflowID {
			delete(m.keys, k)
		}
	}
	return nil
}

func (m *mockConcurrencyKeyStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var reaped int64
	now := time.Now()
	for k, entry := range m.keys {
		if now.After(entry.expiresAt) {
			delete(m.keys, k)
			reaped++
		}
	}
	return reaped, nil
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

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

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

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

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

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

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
	err = store.FailWorkflow(ctx, wfID2, "", "concurrency key conflict: "+key, "", "", nil)
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

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

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

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

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
	err = store.FailWorkflow(ctx, wfID, "", "something went wrong", "", "", nil)
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

// ---------------------------------------------------------------------------
// Concurrency key edge cases (mock store, no DB)
// ---------------------------------------------------------------------------

// TestConcurrencyKeyAlreadyHeldBySameWorkflow verifies that a workflow can
// re-acquire a concurrency key it already holds (idempotent re-acquisition).
func TestConcurrencyKeyAlreadyHeldBySameWorkflow(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// First acquire should succeed.
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-same-key",
	}
	result1 := session.freshAcquireLock(ctx, mod, "same-key", 30000)
	acquired1 := ((result1 >> 8) & 0xFF) != 0
	if !acquired1 {
		t.Fatal("first acquire should succeed")
	}

	// Second acquire with the same workflow and same key should succeed
	// (re-acquisition by the same workflow is allowed).
	result2 := session.freshAcquireLock(ctx, mod, "same-key", 30000)
	acquired2 := ((result2 >> 8) & 0xFF) != 0
	if !acquired2 {
		t.Error("re-acquire by same workflow should succeed")
	}
	if len(session.history) != 2 {
		t.Errorf("expected 2 history entries (2 acquires), got %d", len(session.history))
	}
	// Both history entries should show acquired.
	for i, rec := range session.history {
		if rec.LockAcquired != 1 {
			t.Errorf("history[%d]: expected LockAcquired=1, got %d", i, rec.LockAcquired)
		}
	}
}

// TestAcquireConcurrencyKeyDifferentWorkflowBlocked verifies that when one
// workflow holds a key, a different workflow cannot acquire it.
func TestAcquireConcurrencyKeyDifferentWorkflowBlocked(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	// Use engine-level AcquireConcurrencyKey on the store directly.
	key := "test-key-blocked"
	wf1 := "wf-blocker"
	wf2 := "wf-blocked"

	// Workflow 1 acquires the key.
	acquired, err := cks.AcquireConcurrencyKey(ctx, key, wf1, 30*time.Second)
	if err != nil {
		t.Fatalf("wf1 acquire: %v", err)
	}
	if !acquired {
		t.Fatal("wf1 should acquire the key")
	}

	// Workflow 2 tries to acquire the same key — should be blocked.
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("wf2 acquire: %v", err)
	}
	if acquired {
		t.Error("wf2 should NOT acquire the key (wf1 holds it)")
	}
}

// TestReleaseConcurrencyKeyNonExistent verifies that releasing a concurrency
// key that was never acquired does not return an error (idempotent release).
func TestReleaseConcurrencyKeyNonExistent(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Releasing a key that was never acquired should not error.
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history: make([]EventRecord, 0),
	}

	// Direct store-level call — should succeed.
	err = cks.ReleaseConcurrencyKey(ctx, "non-existent-key")
	if err != nil {
		t.Errorf("releasing non-existent key should succeed, got: %v", err)
	}

	// execSession-level call — should succeed.
	result := session.freshReleaseLock(ctx, mod, "non-existent-key")
	if result != 0 {
		t.Errorf("freshReleaseLock for non-existent key should return 0, got %d", result)
	}

	// Verify event was recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeReleaseLock {
		t.Errorf("expected ReleaseLock event, got %s", session.history[0].EventType)
	}
}

// TestConcurrencyKeyTimeoutExpiration verifies that a concurrency key with
// a very short TTL is released automatically after expiry, allowing another
// workflow to acquire it without manual release.
func TestConcurrencyKeyTimeoutExpiration(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	key := "expiry-key"
	wf1 := "wf-expirer"
	wf2 := "wf-acquirer"

	// Acquire with a very short TTL (10ms) — expires almost immediately.
	acquired, err := cks.AcquireConcurrencyKey(ctx, key, wf1, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("wf1 acquire: %v", err)
	}
	if !acquired {
		t.Fatal("wf1 should acquire the key initially")
	}

	// Wait for TTL to expire.
	time.Sleep(20 * time.Millisecond)

	// Now wf2 should be able to acquire (AcquireConcurrencyKey cleans expired keys).
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("wf2 acquire after expiry: %v", err)
	}
	if !acquired {
		t.Error("wf2 should acquire the key after TTL expiry")
	}
}

// TestConcurrencyKeySessionsReleaseLock verifies that session-level
// freshReleaseLock releases the key allowing another session to acquire it.
func TestConcurrencyKeySessionsReleaseLock(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	key := "session-key"
	wf1 := "wf-session-1"
	wf2 := "wf-session-2"

	session1 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
			workflowID:         wf1,
		},
		history:    make([]EventRecord, 0),
		workflowID: wf1,
	}

	// Session 1 acquires the key.
	result1 := session1.freshAcquireLock(ctx, mod, key, 30000)
	acquired1 := ((result1 >> 8) & 0xFF) != 0
	if !acquired1 {
		t.Fatal("session1 should acquire the key")
	}

	// Session 2 tries — should be blocked.
	result2 := session2AcquireLock(ctx, mod, cks, wf2, key)
	acquired2 := ((result2 >> 8) & 0xFF) != 0
	if acquired2 {
		t.Fatal("session2 should be blocked before release")
	}

	// Session 1 releases the lock.
	releaseResult := session1.freshReleaseLock(ctx, mod, key)
	if releaseResult != 0 {
		t.Errorf("expected successful release (0), got %d", releaseResult)
	}

	// Session 2 tries again — should now acquire.
	_ = result2 // re-use variable
	session2 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
			workflowID:         wf2,
		},
		history:    make([]EventRecord, 0),
		workflowID: wf2,
	}
	result3 := session2.freshAcquireLock(ctx, mod, key, 30000)
	acquired3 := ((result3 >> 8) & 0xFF) != 0
	if !acquired3 {
		t.Error("session2 should acquire after session1 releases")
	}
}

// session2AcquireLock is a helper to test concurrency key blocking.
func session2AcquireLock(ctx context.Context, mod api.Module, cks *mockConcurrencyKeyStore, wfID, key string) int64 {
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
			workflowID:         wfID,
		},
		history:    make([]EventRecord, 0),
		workflowID: wfID,
	}
	return session.freshAcquireLock(ctx, mod, key, 30000)
}

// TestConcurrencyKeyReplayAcquireAlreadyHeld verifies that replaying an
// AcquireLock with an already-held key correctly returns the recorded state.
func TestConcurrencyKeyReplayAcquireAlreadyHeld(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Session 1 acquires and records a history entry.
	session1 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-replay-acquire",
	}
	_ = session1.freshAcquireLock(ctx, mod, "replay-key", 30000)

	// Replay session with the recorded history.
	// On replay, the lock state from history should be returned.
	replaySession := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  session1.history,
		isReplay: true,
	}

	result := replaySession.replayAcquireLock(ctx, mod, "replay-key", 30000)
	acquired := ((result >> 8) & 0xFF) != 0
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("replay acquire: unexpected errCode=%d", errCode)
	}
	if !acquired {
		t.Error("replay acquire should return acquired=true from history")
	}
}

// ---------------------------------------------------------------------------
// Additional concurrency key tests
// ---------------------------------------------------------------------------

// TestAcquireConcurrencyKeyWithAlreadyHeldKey verifies that when a concurrency
// key is already held by one workflow, another workflow cannot acquire it
// (through the store-level interface).
func TestAcquireConcurrencyKeyWithAlreadyHeldKey(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	key := "already-held-key"
	wf1 := "wf-holder"
	wf2 := "wf-contender"

	// First workflow acquires the key.
	acquired, err := cks.AcquireConcurrencyKey(ctx, key, wf1, 30*time.Second)
	if err != nil {
		t.Fatalf("first workflow acquire: %v", err)
	}
	if !acquired {
		t.Fatal("first workflow should acquire the key")
	}

	// Second workflow tries to acquire the same key — should fail.
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("second workflow acquire: %v", err)
	}
	if acquired {
		t.Error("second workflow should NOT acquire the already-held key")
	}

	// After release, the second workflow should be able to acquire.
	err = cks.ReleaseConcurrencyKey(ctx, key)
	if err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("second workflow acquire after release: %v", err)
	}
	if !acquired {
		t.Error("second workflow should acquire after key release")
	}
}

// TestReleaseConcurrencyKeyForNonExistentKey verifies that releasing a
// concurrency key that was never acquired does not return an error (the
// operation is idempotent at the store level).
func TestReleaseConcurrencyKeyForNonExistentKey(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	// Release a key that was never acquired — should not error.
	err := cks.ReleaseConcurrencyKey(ctx, "non-existent-key-for-release")
	if err != nil {
		t.Errorf("releasing non-existent key should succeed, got: %v", err)
	}

	// Release another non-existent key to verify idempotency.
	err = cks.ReleaseConcurrencyKey(ctx, "another-non-existent-key")
	if err != nil {
		t.Errorf("releasing another non-existent key should succeed, got: %v", err)
	}

	// Verify the store is still usable (acquire works after releasing non-existent).
	acquired, err := cks.AcquireConcurrencyKey(ctx, "fresh-key", "wf-fresh", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire after non-existent release: %v", err)
	}
	if !acquired {
		t.Error("should acquire fresh key after releasing non-existent keys")
	}
}

// TestConcurrencyKeyTTLExpiration verifies that a concurrency key with a very
// short TTL is automatically released after expiry, allowing another workflow
// to acquire it. This tests the expiration logic within the mock store.
func TestConcurrencyKeyTTLExpiration(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	key := "ttl-expiration-key"
	wf1 := "wf-ttl-holder"
	wf2 := "wf-ttl-taker"

	// Acquire with a very short TTL (1ms).
	acquired, err := cks.AcquireConcurrencyKey(ctx, key, wf1, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire with TTL: %v", err)
	}
	if !acquired {
		t.Fatal("should acquire key with short TTL")
	}

	// Immediately try to acquire from another workflow — key is still held.
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("immediate re-acquire: %v", err)
	}
	if acquired {
		t.Error("second workflow should NOT acquire before TTL expiry")
	}

	// Wait for TTL to expire.
	time.Sleep(5 * time.Millisecond)

	// Now the second workflow should be able to acquire.
	acquired, err = cks.AcquireConcurrencyKey(ctx, key, wf2, 30*time.Second)
	if err != nil {
		t.Fatalf("re-acquire after TTL expiry: %v", err)
	}
	if !acquired {
		t.Error("second workflow should acquire after TTL expiry")
	}
}

// ---------------------------------------------------------------------------
// ReleaseWorkflowConcurrencyKeys tests
// ---------------------------------------------------------------------------

// TestReleaseWorkflowConcurrencyKeys verifies that all concurrency keys held
// by a specific workflow are released when ReleaseWorkflowConcurrencyKeys is
// called. Other workflows' keys should remain unaffected.
func TestReleaseWorkflowConcurrencyKeys(t *testing.T) {
	ctx := context.Background()
	cks := newMockConcurrencyKeyStore()

	// Acquire keys for two different workflows.
	_, err := cks.AcquireConcurrencyKey(ctx, "key-wf1-a", "wf-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-wf1-a: %v", err)
	}
	_, err = cks.AcquireConcurrencyKey(ctx, "key-wf1-b", "wf-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-wf1-b: %v", err)
	}
	_, err = cks.AcquireConcurrencyKey(ctx, "key-wf2-a", "wf-2", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-wf2-a: %v", err)
	}

	// Release all keys for wf-1.
	err = cks.ReleaseWorkflowConcurrencyKeys(ctx, "wf-1")
	if err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
	}

	// After release, wf-1 should be able to re-acquire its keys.
	acquired, err := cks.AcquireConcurrencyKey(ctx, "key-wf1-a", "wf-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("re-acquire key-wf1-a: %v", err)
	}
	if !acquired {
		t.Error("wf-1 should be able to re-acquire key-wf1-a after release")
	}

	// wf-2 should still hold its key.
	acquired, err = cks.AcquireConcurrencyKey(ctx, "key-wf2-a", "wf-3", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-wf2-a with wf-3: %v", err)
	}
	if acquired {
		t.Error("wf-3 should NOT acquire key-wf2-a (wf-2 still holds it)")
	}

	// Verify wf-1's other key is also free.
	acquired, err = cks.AcquireConcurrencyKey(ctx, "key-wf1-b", "wf-3", 30*time.Minute)
	if err != nil {
		t.Fatalf("re-acquire key-wf1-b: %v", err)
	}
	if !acquired {
		t.Error("key-wf1-b should be free after wf-1's keys were released")
	}
}

// ---------------------------------------------------------------------------
// ReapExpiredConcurrencyKeys tests
// ---------------------------------------------------------------------------

// TestReapExpiredConcurrencyKeys verifies that ReapExpiredConcurrencyKeys
// removes expired keys from the store and returns the count of reaped keys.
func TestReapExpiredConcurrencyKeys(t *testing.T) {
	ctx := context.Background()
	cks := newMockConcurrencyKeyStore()

	// Acquire a key with a normal TTL (should not expire during test).
	_, err := cks.AcquireConcurrencyKey(ctx, "live-key", "wf-live", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire live-key: %v", err)
	}

	// Acquire a key with a very short TTL (will expire quickly).
	_, err = cks.AcquireConcurrencyKey(ctx, "expired-key", "wf-expired", 1*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire expired-key: %v", err)
	}

	// Wait for the short TTL to expire.
	time.Sleep(5 * time.Millisecond)

	// Reap expired keys.
	reaped, err := cks.ReapExpiredConcurrencyKeys(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if reaped < 1 {
		t.Errorf("expected at least 1 reaped key, got %d", reaped)
	}

	// The live key should still be present.
	acquired, err := cks.AcquireConcurrencyKey(ctx, "live-key", "wf-other", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire live-key after reap: %v", err)
	}
	if acquired {
		t.Error("live-key should still be held by wf-live (was not expired)")
	}

	// The expired key should now be available for acquisition.
	acquired, err = cks.AcquireConcurrencyKey(ctx, "expired-key", "wf-new", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire expired-key after reap: %v", err)
	}
	if !acquired {
		t.Error("expired-key should be free after reap")
	}
}

// TestReapExpiredConcurrencyKeysNoExpiredKeys verifies that ReapExpiredConcurrencyKeys
// returns 0 when there are no expired keys to reap.
func TestReapExpiredConcurrencyKeysNoExpiredKeys(t *testing.T) {
	ctx := context.Background()
	cks := newMockConcurrencyKeyStore()

	// Acquire several keys with very long TTLs (none will expire during test).
	_, err := cks.AcquireConcurrencyKey(ctx, "key-1", "wf-1", 24*time.Hour)
	if err != nil {
		t.Fatalf("acquire key-1: %v", err)
	}
	_, err = cks.AcquireConcurrencyKey(ctx, "key-2", "wf-2", 24*time.Hour)
	if err != nil {
		t.Fatalf("acquire key-2: %v", err)
	}

	// Reap — none should be expired.
	reaped, err := cks.ReapExpiredConcurrencyKeys(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if reaped != 0 {
		t.Errorf("expected 0 reaped keys (none expired), got %d", reaped)
	}

	// Both keys should still be held.
	acquired, err := cks.AcquireConcurrencyKey(ctx, "key-1", "wf-other", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-1 after reap: %v", err)
	}
	if acquired {
		t.Error("key-1 should still be held by wf-1")
	}

	acquired, err = cks.AcquireConcurrencyKey(ctx, "key-2", "wf-other", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire key-2 after reap: %v", err)
	}
	if acquired {
		t.Error("key-2 should still be held by wf-2")
	}
}

// ---------------------------------------------------------------------------
// Acquire concurrency key with expired key
// ---------------------------------------------------------------------------

// TestAcquireConcurrencyKeyWithExpiredKey verifies that an expired concurrency
// key can be acquired by a new workflow through the execSession-level
// freshAcquireLock, which delegates to the store's AcquireConcurrencyKey
// (which cleans up expired keys during acquisition).
func TestAcquireConcurrencyKeyWithExpiredKey(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()

	// Workflow 1 acquires the key with a very short TTL.
	session1 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-expirer",
	}
	result1 := session1.freshAcquireLock(ctx, mod, "expiring-key", 1 /* 1ms TTL */)
	acquired1 := ((result1 >> 8) & 0xFF) != 0
	if !acquired1 {
		t.Fatal("wf-expirer should acquire the key initially")
	}

	// Wait for TTL to expire.
	time.Sleep(5 * time.Millisecond)

	// Now a new workflow should be able to acquire the key (expired keys are
	// cleaned up during AcquireConcurrencyKey).
	session2 := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-taker",
	}
	result2 := session2.freshAcquireLock(ctx, mod, "expiring-key", 30000)
	acquired2 := ((result2 >> 8) & 0xFF) != 0
	errCode2 := byte(result2 & 0xFF)
	if errCode2 != 0 {
		t.Fatalf("unexpected errCode=%d", errCode2)
	}
	if !acquired2 {
		t.Error("wf-taker should acquire the expired key after TTL expiry")
	}

	// Verify both events are recorded in history.
	if len(session1.history) < 1 || session1.history[0].LockAcquired != 1 {
		t.Error("session1 history should show lock acquired")
	}
	if len(session2.history) < 1 || session2.history[0].LockAcquired != 1 {
		t.Error("session2 history should show lock acquired (expired key was re-acquired)")
	}
}

// ---------------------------------------------------------------------------
// Race between acquire and reap
// ---------------------------------------------------------------------------

// TestRaceBetweenAcquireAndReap verifies that concurrent calls to
// AcquireConcurrencyKey and ReapExpiredConcurrencyKeys do not deadlock or
// produce inconsistent state. The mock store is safe for concurrent access
// via a mutex, so these operations should complete without error.
func TestRaceBetweenAcquireAndReap(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	// Pre-populate the store with an already-expired key.
	cks.mu.Lock()
	cks.keys["stale-key"] = concurrencyKeyEntry{
		workflowID: "wf-stale",
		expiresAt:  time.Now().Add(-1 * time.Second),
	}
	cks.mu.Unlock()

	// Acquire a key that won't expire during the test.
	_, err := cks.AcquireConcurrencyKey(ctx, "live-key", "wf-live", 30*time.Minute)
	if err != nil {
		t.Fatalf("acquire live-key: %v", err)
	}

	// Run 10 concurrent goroutines that alternate between acquire and reap.
	var wg sync.WaitGroup
	errs := make(chan error, 20)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Acquire a unique key.
			key := fmt.Sprintf("race-key-%d", i)
			_, err := cks.AcquireConcurrencyKey(ctx, key, fmt.Sprintf("wf-race-%d", i), 30*time.Second)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d acquire: %w", i, err)
			}
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cks.ReapExpiredConcurrencyKeys(ctx)
			if err != nil {
				errs <- fmt.Errorf("reap: %w", err)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}

	// Verify the live key is still held by wf-live.
	acquired, err := cks.AcquireConcurrencyKey(ctx, "live-key", "wf-other", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire live-key after race: %v", err)
	}
	if acquired {
		t.Error("live-key should still be held by wf-live after concurrent operations")
	}

	// Verify the stale key was reaped and is now available.
	acquired, err = cks.AcquireConcurrencyKey(ctx, "stale-key", "wf-new", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire stale-key after race: %v", err)
	}
	if !acquired {
		t.Error("stale-key should be available after concurrent reap")
	}

	// Verify all 10 race keys were acquired.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("race-key-%d", i)
		acquired, err := cks.AcquireConcurrencyKey(ctx, key, fmt.Sprintf("wf-other-%d", i), 30*time.Second)
		if err != nil {
			t.Fatalf("verify race-key-%d: %v", i, err)
		}
		if acquired {
			t.Errorf("race-key-%d should still be held by original acquirer", i)
		}
	}
}
