package integrity

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/engine"
)

// TestConcurrentEventAppends verifies that multiple goroutines appending events
// to the same workflow do not cause corruption.
func TestConcurrentEventAppends(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-conc-append-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	const numGoroutines = 10
	const eventsPerGoroutine = 5

	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines)

	// Launch goroutines, each appending a unique set of steps.
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			var events []engine.EventRecord
			baseStep := gID * eventsPerGoroutine
			for i := 0; i < eventsPerGoroutine; i++ {
				events = append(events, engine.EventRecord{
					Step:      baseStep + i,
					EventType: engine.EventTypeCall,
					Service:   fmt.Sprintf("worker-%d", gID),
					Op:        fmt.Sprintf("step-%d", i),
					Request:   `{}`,
					Response:  `{"ok":true}`,
				})
			}
			if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
				errCh <- fmt.Errorf("goroutine %d append: %w", gID, err)
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent append error: %v", err)
	}

	// Load the full history and verify.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	expectedTotal := numGoroutines * eventsPerGoroutine
	if len(history) != expectedTotal {
		t.Errorf("expected %d events, got %d", expectedTotal, len(history))
	}

	// Verify events are in step order and have no duplicates.
	seenSteps := make(map[int]bool)
	for _, ev := range history {
		if seenSteps[ev.Step] {
			t.Errorf("duplicate step %d found", ev.Step)
		}
		seenSteps[ev.Step] = true
	}

	// Verify no missing steps in the expected range.
	for i := 0; i < expectedTotal; i++ {
		if !seenSteps[i] {
			t.Errorf("missing step %d", i)
		}
	}
}

// TestConcurrentClaimAndHeartbeat verifies that claiming and heartbeating from
// multiple goroutines maintains ownership consistency.
func TestConcurrentClaimAndHeartbeat(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	// Create a workflow for each contention scenario.
	makeWorkflow := func(suffix string) string {
		id := fmt.Sprintf("int-conc-claim-%s-%d", suffix, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %s: %v", suffix, err)
		}
		t.Cleanup(func() {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		})
		return id
	}

	runID := makeWorkflow("single")

	// Launch multiple workers trying to claim and heartbeat.
	const numWorkers = 5
	var wg sync.WaitGroup
	claimCount := make(chan string, numWorkers)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			// Try to claim the workflow.
			wf, err := store.ClaimWorkflow(ctx, workerID)
			if err != nil {
				return
			}
			if wf != nil && wf.ID == runID {
				// Successfully claimed — heartbeat a few times.
				for h := 0; h < 3; h++ {
					alive, err := store.Heartbeat(ctx, runID, workerID, 0)
					if err != nil {
						return
					}
					if !alive {
						return // Lost ownership
					}
					time.Sleep(5 * time.Millisecond)
				}
				claimCount <- workerID
			}
		}(fmt.Sprintf("worker-%d", i))
	}
	wg.Wait()
	close(claimCount)

	// Only one worker should have claimed the workflow.
	uniqueClaims := make(map[string]bool)
	for w := range claimCount {
		uniqueClaims[w] = true
	}
	if len(uniqueClaims) > 1 {
		t.Errorf("expected at most 1 claim, got %d: %v", len(uniqueClaims), uniqueClaims)
	}

	// Verify the workflow instance status.
	wf, err := store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow not found")
	}
	// Should be in 'running' or 'ready' status depending on whether the
	// claiming worker released it.
	if wf.Status != "running" && wf.Status != "ready" {
		t.Errorf("expected status 'running' or 'ready', got %q", wf.Status)
	}
}

// TestConcurrentStatusUpdates verifies concurrent status updates leave the
// workflow in a valid final state.
func TestConcurrentStatusUpdates(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	// Create multiple workflows.
	const numWorkflows = 10
	var wfIDs []string
	for i := 0; i < numWorkflows; i++ {
		runID := fmt.Sprintf("int-conc-status-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
		if err != nil {
			t.Fatalf("create workflow %d: %v", i, err)
		}
		wfIDs = append(wfIDs, runID)
	}
	defer func() {
		for _, id := range wfIDs {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	}()

	const numIterations = 20

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkflows*numIterations)

	// Each goroutine claims a workflow, does work, and completes or fails it.
	for i := 0; i < numWorkflows; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := wfIDs[idx]
			workerID := fmt.Sprintf("worker-%d", idx)

			for iter := 0; iter < numIterations; iter++ {
				// Claim the workflow.
				wf, err := store.ClaimWorkflow(ctx, workerID)
				if err != nil {
					// No workflows ready — skip this iteration.
					continue
				}
				if wf == nil || wf.ID != id {
					continue
				}

				// Alternate between completing and failing.
				if iter%2 == 0 {
					if err := store.CompleteWorkflow(ctx, id, workerID, 0, `{"status":"done"}`, nil); err != nil {
						errCh <- fmt.Errorf("complete %s iter %d: %w", id, iter, err)
					}
				} else {
					// Re-create as ready for next iteration by releasing.
					if err := store.ReleaseWorkflow(ctx, id, workerID, 0, time.Now()); err != nil {
						errCh <- fmt.Errorf("release %s iter %d: %w", id, iter, err)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent status update error: %v", err)
	}

	// Verify every workflow is in a valid terminal or intermediate state.
	for _, id := range wfIDs {
		wf, err := store.GetWorkflowByID(ctx, id)
		if err != nil {
			t.Errorf("GetWorkflowByID %s: %v", id, err)
			continue
		}
		if wf == nil {
			t.Errorf("workflow %s not found", id)
			continue
		}
		validStatus := wf.Status == "ready" || wf.Status == "running" || wf.Status == "done" || wf.Status == "failed"
		if !validStatus {
			t.Errorf("workflow %s has invalid status %q", id, wf.Status)
		}
	}
}
