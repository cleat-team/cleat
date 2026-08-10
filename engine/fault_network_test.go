package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestFaultNetworkPartition simulates a network partition via transaction
// isolation, verifying that workflows pause and resume when connectivity is
// restored.
func TestFaultNetworkPartition(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	deployFaultTestDef(t, store)

	runID := fmt.Sprintf("test-network-partition-%d", time.Now().UnixNano())
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID); err != nil {
		t.Fatalf("insert test instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Simulate a network partition by preventing claim via a long-running
	// transaction that holds a conflicting lock.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("Begin partition tx: %v", err)
	}

	// Lock the workflow instance row to simulate one side of a partition.
	_, err = tx.Exec(`SELECT id FROM workflow_instances WHERE id = $1 FOR UPDATE`, runID)
	if err != nil {
		tx.Rollback()
		t.Fatalf("Lock instance: %v", err)
	}

	// Attempt to claim — should not block immediately since we use SKIP LOCKED,
	// but should return nil (no claims available because the row is locked).
	claimDone := make(chan struct{})
	var claimed bool
	go func() {
		wf, err := store.ClaimWorkflow(ctx, "partition-worker")
		if err == nil && wf != nil {
			claimed = true
		}
		close(claimDone)
	}()

	select {
	case <-claimDone:
		// Claim completed — it should not have succeeded since the row is locked.
		if claimed {
			t.Error("Claim succeeded during simulated partition; expected failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Claim blocked during simulated partition; SKIP LOCKED should prevent blocking")
	}

	// "Heal" the partition by committing the blocking transaction.
	tx.Commit()

	// Now claim should succeed.
	wf, err := store.ClaimWorkflow(ctx, "partition-worker")
	if err != nil {
		t.Fatalf("Claim after partition heal: %v", err)
	}
	if wf == nil {
		t.Fatal("Expected to claim workflow after partition heal, got nil")
	}

	t.Logf("Network partition test passed: workflow %s claimed after partition healed", wf.ID)
}

// TestFaultSlowNetwork simulates a slow network via long-running queries,
// verifying that heartbeats continue to work.
func TestFaultSlowNetwork(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	deployFaultTestDef(t, store)

	runID := fmt.Sprintf("test-slow-network-%d", time.Now().UnixNano())
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID); err != nil {
		t.Fatalf("insert test instance: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Claim the workflow first.
	wf, err := store.ClaimWorkflow(ctx, "slow-worker")
	if err != nil {
		t.Fatalf("Initial claim: %v", err)
	}
	if wf == nil {
		t.Fatal("Expected to claim workflow")
	}

	// Simulate slow network by running a concurrent slow query.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run a pg_sleep to simulate a slow network/stalled query.
		db.ExecContext(ctx, `SELECT pg_sleep(0.5)`)
	}()

	// While the slow query is running, heartbeat should still succeed.
	alive, err := store.Heartbeat(ctx, runID, "slow-worker", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat during slow network: %v", err)
	}
	if !alive {
		t.Error("Expected heartbeat to succeed during slow network conditions")
	}

	wg.Wait()

	// Verify heartbeat still works after slow query completes.
	alive, err = store.Heartbeat(ctx, runID, "slow-worker", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat after slow network: %v", err)
	}
	if !alive {
		t.Error("Expected heartbeat to succeed after slow network")
	}

	// Verify that loading event history works during slow conditions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		db.ExecContext(ctx, `SELECT pg_sleep(0.3)`)
	}()

	events := []EventRecord{
		{Step: 1, EventType: "call", Service: "slow", Op: "test", Request: `{}`, Response: `{}`},
	}
	err = store.AppendEventHistoryBatch(ctx, runID, events)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch during slow network: %v", err)
	}

	wg.Wait()

	t.Logf("Slow network test passed: heartbeats and event operations succeeded")
}
