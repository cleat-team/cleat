package host

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Concurrency key tests against PostgresStore directly.
// These require a real PostgreSQL database. Set DURABLE_TEST_DB to run.
// Example: DURABLE_TEST_DB="postgres://localhost:5432/cleat?sslmode=disable" go test -v -run TestConcurrency
// ---------------------------------------------------------------------------

func TestConcurrencyKeyAcquireRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency key test in short mode")
	}

	db := testDB(t)
	defer db.Close()

	// Ensure concurrency_keys table exists.
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY,
		key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create concurrency_keys table: %v", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)

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

	db := testDB(t)
	defer db.Close()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY,
		key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create concurrency_keys table: %v", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)

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

	db := testDB(t)
	defer db.Close()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY,
		key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create concurrency_keys table: %v", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)

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

	db := testDB(t)
	defer db.Close()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY,
		key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create concurrency_keys table: %v", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)

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

	db := testDB(t)
	defer db.Close()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY,
		key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create concurrency_keys table: %v", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)

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
