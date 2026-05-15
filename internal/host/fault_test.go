package host

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/host/testutil"
)

// These tests require a real PostgreSQL database. Set CLEAT_TEST_DB to run.
// Example: CLEAT_TEST_DB="postgres://localhost:5432/cleat?sslmode=disable" go test -v -run TestFault

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)

	// Clean up any leftover test data from previous runs.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'test-%' OR workflow_id LIKE 'wf-%' OR workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE 'test-%' OR workflow_id LIKE 'wf-%' OR workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE 'test-%' OR workflow_id LIKE 'wf-%' OR workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE 'test-%' OR workflow_id LIKE 'wf-%' OR workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'test-%' OR id LIKE 'wf-%' OR id LIKE 'int-%'`)
	return db
}

// TestFaultConcurrentClaim verifies that SKIP LOCKED prevents duplicate claims
// when multiple workers compete for the same workflow.
func TestFaultConcurrentClaim(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()




	// Create a test instance.
	runID := fmt.Sprintf("test-concurrent-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID)
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	var wg sync.WaitGroup
	claims := make(chan string, 10)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			wf, err := store.ClaimWorkflow(ctx, workerID)
			if err != nil {
				return
			}
			if wf != nil {
				claims <- workerID
			}
		}(fmt.Sprintf("worker-%d", i))
	}
	wg.Wait()
	close(claims)

	if len(claims) != 1 {
		t.Errorf("Expected exactly 1 claim, got %d", len(claims))
	}
}

// TestFaultEventHistoryIdempotency verifies that ON CONFLICT DO NOTHING
// prevents duplicate events from being inserted.
func TestFaultEventHistoryIdempotency(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()

	runID := fmt.Sprintf("test-idempotent-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID)
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	events := []EventRecord{
		{Step: 1, EventType: "call", Service: "test", Op: "ping", Request: `{}`, Response: `{"ok":true}`},
		{Step: 2, EventType: "call", Service: "test", Op: "pong", Request: `{}`, Response: `{"ok":true}`},
	}

	// Insert the same batch twice.
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("First append: %v", err)
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("Second append: %v", err)
	}

	// Verify only 2 events exist.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("Load history: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("Expected 2 events after duplicate append, got %d", len(history))
	}
}

// TestFaultReapStaleInstances verifies that the zombie reaper reclaims
// instances with stale heartbeats.
func TestFaultReapStaleInstances(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()




	runID := fmt.Sprintf("test-reap-%d", time.Now().UnixNano())
	// Insert directly with an old heartbeat.
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, assigned_to, heartbeat_at)
		VALUES ($1, 'test', 1, 'running', '{}', 'dead-worker', now() - interval '2 minutes')
		ON CONFLICT DO NOTHING`, runID)
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Reap with a short timeout.
	reaped, err := store.ReapStaleInstances(ctx, 10*time.Second)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if reaped != 1 {
		t.Errorf("Expected 1 reaped instance, got %d", reaped)
	}

	// Verify the instance is back to 'ready'.
	var status string
	db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, runID).Scan(&status)
	if status != "ready" {
		t.Errorf("Expected status 'ready', got %q", status)
	}
}

// TestFaultHeartbeatOwnership verifies that a worker loses ownership when
// another worker claims the instance.
func TestFaultHeartbeatOwnership(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()




	runID := fmt.Sprintf("test-heartbeat-%d", time.Now().UnixNano())
	db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID)
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Worker A claims the instance.
	wfA, err := store.ClaimWorkflow(ctx, "worker-a")
	if err != nil {
		t.Fatalf("Worker A claim: %v", err)
	}
	if wfA == nil || wfA.ID != runID {
		t.Fatal("Worker A should have claimed the instance")
	}

	// Worker A heartbeats — should succeed.
	alive, err := store.Heartbeat(ctx, runID, "worker-a", wfA.Generation)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !alive {
		t.Error("Worker A heartbeat should succeed")
	}

	// Worker B tries to heartbeat — should fail.
	alive, err = store.Heartbeat(ctx, runID, "worker-b", wfA.Generation)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if alive {
		t.Error("Worker B heartbeat should fail (not owner)")
	}
}

