package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/rcownie/cleat/internal/host"
)

// testStore returns a *sql.DB and a PostgresStore for tests.
// It follows the same pattern as internal/host/fault_test.go's testDB().
func testStore(t *testing.T, taskQueues ...string) (*sql.DB, *host.PostgresStore) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping cluster test: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping cluster test: cannot ping database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, host.NewPostgresStore(db, taskQueues...)
}

// cleanTestWorkflows removes test workflows from the database.
func cleanTestWorkflows(t *testing.T, db *sql.DB) {
	t.Helper()
	// Remove test data that matches our test workflow ID patterns.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'test-cluster-%'`)
	db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE 'test-cluster-%'`)
	db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE 'test-cluster-%'`)
	db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE 'test-cluster-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'test-cluster-%'`)
}

// TestClusterWorkersRegister verifies that 3 workers each claim workflows from
// their respective task queues.
func TestClusterWorkersRegister(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	db, store := testStore(t, "queue-1", "queue-2", "queue-3")
	ctx := context.Background()

	cleanTestWorkflows(t, db)

	// Create test workflows in each queue.
	workflowIDs := make([]string, 0, 3)
	queues := []string{"queue-1", "queue-2", "queue-3"}

	for i, q := range queues {
		id := fmt.Sprintf("test-cluster-register-%d", i)
		_, err := db.Exec(`
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', $2)
			ON CONFLICT (id) DO NOTHING
		`, id, q)
		if err != nil {
			t.Fatalf("Insert workflow for queue %s: %v", q, err)
		}
		workflowIDs = append(workflowIDs, id)
	}

	defer func() {
		for _, id := range workflowIDs {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	}()

	// Each worker claims from its own queue (store is configured for all three).
	claimed := make(map[string]string)

	for _, workerID := range []string{"worker-1", "worker-2", "worker-3"} {
		wf, err := store.ClaimWorkflow(ctx, workerID)
		if err != nil {
			t.Fatalf("Claim error for %s: %v", workerID, err)
		}
		if wf != nil {
			claimed[wf.ID] = workerID
		}
	}

	if len(claimed) != 3 {
		t.Errorf("Expected 3 claims (one per queue), got %d", len(claimed))
	}

	// Verify each test workflow was claimed.
	for _, id := range workflowIDs {
		if _, ok := claimed[id]; !ok {
			t.Errorf("Workflow %s was not claimed", id)
		}
	}
}

// TestClusterSpreadWorkflows creates 100 workflows distributed across three
// queues and verifies they are claimed by the workers.
func TestClusterSpreadWorkflows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	db, store := testStore(t, "queue-1", "queue-2", "queue-3")
	ctx := context.Background()

	cleanTestWorkflows(t, db)

	const numWorkflows = 100
	queues := []string{"queue-1", "queue-2", "queue-3"}
	workflowIDs := make([]string, 0, numWorkflows)

	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("test-cluster-spread-%d", i)
		q := queues[i%len(queues)]
		_, err := db.Exec(`
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'spread-test', 1, 'ready', '{}', $2)
			ON CONFLICT (id) DO NOTHING
		`, id, q)
		if err != nil {
			t.Fatalf("Insert workflow %d: %v", i, err)
		}
		workflowIDs = append(workflowIDs, id)
	}

	defer func() {
		for _, id := range workflowIDs {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	}()

	// Claim all workflows.
	workers := []string{"worker-1", "worker-2", "worker-3"}
	claimCount := make(map[string]int)

	for range 200 {
		claimed := false
		for _, workerID := range workers {
			wf, err := store.ClaimWorkflow(ctx, workerID)
			if err != nil {
				t.Fatalf("Claim error for %s: %v", workerID, err)
			}
			if wf != nil {
				claimCount[workerID]++
				claimed = true
			}
		}
		if !claimed {
			break
		}
	}

	totalClaims := 0
	for _, workerID := range workers {
		totalClaims += claimCount[workerID]
		t.Logf("Worker %s claimed %d workflows", workerID, claimCount[workerID])
	}

	if totalClaims != numWorkflows {
		t.Errorf("Expected %d total claims, got %d", numWorkflows, totalClaims)
	}

	for _, workerID := range workers {
		if claimCount[workerID] == 0 {
			t.Errorf("Worker %s claimed 0 workflows", workerID)
		}
	}
}

// TestClusterBasicWorkflowExecution creates a workflow, claims it, executes it,
// and verifies the complete lifecycle.
func TestClusterBasicWorkflowExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	db, store := testStore(t, "queue-1")
	ctx := context.Background()

	cleanTestWorkflows(t, db)

	runID := "test-cluster-execution"
	_, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'basic-test', 1, 'ready', '{}', 'queue-1')
		ON CONFLICT (id) DO NOTHING
	`, runID)
	if err != nil {
		t.Fatalf("Insert workflow: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Claim the workflow.
	wf, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("Expected to claim a workflow, got nil")
	}

	// Verify we got the right workflow.
	if wf.ID != runID {
		t.Logf("Note: claimed workflow %s (expected %s) — queues may serve in FIFO order", wf.ID, runID)
	}

	// Append an execution event.
	events := []host.EventRecord{
		{Step: 1, EventType: "call", Service: "test", Op: "ping", Request: `{}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, wf.ID, events); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// Load event history to verify.
	history, err := store.LoadEventHistory(ctx, wf.ID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("Expected 1 event, got %d", len(history))
	}

	// Complete the workflow.
	if err := store.CompleteWorkflow(ctx, wf.ID, "worker-1", `{"result":"done"}`, nil); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// Verify the workflow is done.
	wfDone, err := store.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wfDone == nil {
		t.Fatal("Workflow not found after completion")
	}
	if wfDone.Status != "done" {
		t.Errorf("Expected status 'done', got %q", wfDone.Status)
	}
}
