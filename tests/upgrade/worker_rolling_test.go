package upgrade

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/lib/pq"
)

// TestRollingWorkerRestart simulates restarting workers one at a time and
// verifies that all workflows complete without downtime.
func TestRollingWorkerRestart(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	const numWorkflows = 50
	const numWorkers = 5

	// Create workflow instances.
	var wfIDs []string
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("upg-rolling-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %d: %v", i, err)
		}
		wfIDs = append(wfIDs, id)
	}
	defer func() {
		for _, id := range wfIDs {
			db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	}()

	// Simulate rolling restart:
	// Phase 1: All workers process workflows.
	// Phase 2: Workers restart one at a time (simulated by stopping claim).
	// Phase 3: Verify all workflows eventually complete.

	var completed int64
	errCh := make(chan error, numWorkflows*2)

	// Worker function: continuously claims and completes workflows.
	workerFn := func(workerID string, stopCh <-chan struct{}, wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			wf, err := store.ClaimWorkflow(ctx, workerID)
			if err != nil {
				// Transient error — retry.
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if wf == nil {
				// No ready workflows — wait and retry.
				time.Sleep(2 * time.Millisecond)
				continue
			}

			// Every store call below passes wf.Generation, the value the
			// claim just handed back -- not a literal 0. ClaimWorkflow bumps
			// the generation, so a hardcoded 0 loses the fence from the second
			// claim onward and the store correctly refuses the write. These
			// tests used to send 0 and report the refusal as a worker error,
			// which is the fence working.
			//
			// Append a completion event.
			rec := engine.EventRecord{
				Step:      0,
				EventType: engine.EventTypeCall,
				Service:   "svc",
				Op:        "complete",
				Request:   `{}`,
				Response:  `{"ok":true}`,
			}
			if err := store.AppendEventHistory(ctx, wf.ID, rec); err != nil {
				errCh <- fmt.Errorf("worker %s append: %w", workerID, err)
				// Release the workflow so another worker can pick it up.
				store.ReleaseWorkflow(ctx, wf.ID, workerID, wf.Generation, time.Now())
				continue
			}

			if err := store.CompleteWorkflow(ctx, wf.ID, workerID, wf.Generation, `{"status":"done"}`, nil); err != nil {
				errCh <- fmt.Errorf("worker %s complete: %w", workerID, err)
				store.ReleaseWorkflow(ctx, wf.ID, workerID, wf.Generation, time.Now())
				continue
			}

			atomic.AddInt64(&completed, 1)
		}
	}

	// Start all workers.
	var workerWg sync.WaitGroup
	stopChs := make([]chan struct{}, numWorkers)
	for i := 0; i < numWorkers; i++ {
		stopChs[i] = make(chan struct{})
		workerWg.Add(1)
		go workerFn(fmt.Sprintf("worker-%d", i), stopChs[i], &workerWg)
	}

	// Let workers process for a brief period.
	time.Sleep(100 * time.Millisecond)

	// Simulate rolling restart: stop workers one at a time.
	for i := 0; i < numWorkers; i++ {
		close(stopChs[i])
		time.Sleep(20 * time.Millisecond) // Brief delay between restarts.
	}

	// Wait for all workers to stop.
	workerWg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("rolling restart error: %v", err)
	}

	// Check how many workflows completed.
	//
	// This used to be a t.Logf and nothing else, so the test passed while
	// printing "0/50 workflows completed" -- it asserted that the workers did
	// not error, not that they did any work. With the fence bug above, 0 was
	// the actual number.
	//
	// The property worth checking is that a rolling restart loses no work, so
	// drain whatever is still ready with one final worker and then require
	// every workflow to be done.
	finalCompleted := atomic.LoadInt64(&completed)
	t.Logf("Rolling restart test: %d/%d workflows completed before draining, by %d workers",
		finalCompleted, numWorkflows, numWorkers)
	if finalCompleted == 0 {
		t.Fatalf("no workflows completed at all during the rolling restart")
	}

	for i := 0; i < numWorkflows*2; i++ {
		wf, err := store.ClaimWorkflow(ctx, "drain-worker")
		if err != nil || wf == nil {
			break
		}
		if err := store.CompleteWorkflow(ctx, wf.ID, "drain-worker", wf.Generation, `{"status":"done"}`, nil); err != nil {
			t.Fatalf("drain %s: %v", wf.ID, err)
		}
	}

	var notDone int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_instances
		WHERE id = ANY($1) AND status <> 'done'`, pq.Array(wfIDs)).Scan(&notDone); err != nil {
		t.Fatalf("count unfinished: %v", err)
	}
	if notDone != 0 {
		t.Errorf("%d of %d workflows did not reach 'done' across the rolling restart",
			notDone, numWorkflows)
	}
}

// TestRollingRestartNoDuplicateExecution verifies that no workflow is executed
// twice (duplicate completions) during rolling restart simulation.
func TestRollingRestartNoDuplicateExecution(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	const numWorkflows = 30
	const numWorkers = 4

	// Create workflow instances.
	type wfState struct {
		id           string
		executeCount int32
	}
	var workflows []wfState
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("upg-nodup-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %d: %v", i, err)
		}
		workflows = append(workflows, wfState{id: id})
	}
	defer func() {
		for _, wf := range workflows {
			db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, wf.id)
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wf.id)
		}
	}()

	errCh := make(chan error, numWorkflows*2)
	dupCh := make(chan string, numWorkflows)

	// Worker function: claims, processes, and completes.
	workerFn := func(workerID string, stopCh <-chan struct{}, wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			// Claim with the worker's sticky preference.
			wf, err := store.ClaimWorkflow(ctx, workerID)
			if err != nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if wf == nil {
				time.Sleep(2 * time.Millisecond)
				continue
			}

			// Find the workflow state.
			var state *wfState
			for i := range workflows {
				if workflows[i].id == wf.ID {
					state = &workflows[i]
					break
				}
			}
			if state == nil {
				continue
			}

			// Atomically check and increment execute count.
			prev := atomic.AddInt32(&state.executeCount, 1)
			if prev > 1 {
				dupCh <- fmt.Sprintf("workflow %s executed %d times", wf.ID, prev)
			}

			// Append event and complete.
			rec := engine.EventRecord{
				Step:      0,
				EventType: engine.EventTypeCall,
				Service:   "svc",
				Op:        "execute",
				Request:   `{}`,
				Response:  `{"ok":true}`,
			}
			if err := store.AppendEventHistory(ctx, wf.ID, rec); err != nil {
				errCh <- fmt.Errorf("append: %w", err)
				store.ReleaseWorkflow(ctx, wf.ID, workerID, wf.Generation, time.Now())
				atomic.AddInt32(&state.executeCount, -1)
				continue
			}

			if err := store.CompleteWorkflow(ctx, wf.ID, workerID, wf.Generation, `{"status":"done"}`, nil); err != nil {
				errCh <- fmt.Errorf("complete: %w", err)
				store.ReleaseWorkflow(ctx, wf.ID, workerID, wf.Generation, time.Now())
				atomic.AddInt32(&state.executeCount, -1)
				continue
			}
		}
	}

	// Start workers.
	var workerWg sync.WaitGroup
	stopChs := make([]chan struct{}, numWorkers)
	for i := 0; i < numWorkers; i++ {
		stopChs[i] = make(chan struct{})
		workerWg.Add(1)
		go workerFn(fmt.Sprintf("restart-worker-%d", i), stopChs[i], &workerWg)
	}

	// Let workers process for a period, simulating rolling restarts.
	for i := 0; i < numWorkers; i++ {
		time.Sleep(50 * time.Millisecond)
		close(stopChs[i])

		// Wait a moment, then start a replacement worker.
		time.Sleep(10 * time.Millisecond)
		newIdx := i + numWorkers
		stopCh := make(chan struct{})
		stopChs = append(stopChs, stopCh)
		workerWg.Add(1)
		go workerFn(fmt.Sprintf("restart-worker-%d", newIdx), stopCh, &workerWg)
	}

	// Let final workers drain remaining workflows.
	time.Sleep(200 * time.Millisecond)

	// Stop all remaining workers.
	for _, ch := range stopChs[numWorkers:] {
		close(ch)
	}
	workerWg.Wait()
	close(errCh)
	close(dupCh)

	for err := range errCh {
		t.Errorf("worker error: %v", err)
	}

	// Check for duplicate executions.
	var dupCount int
	for dup := range dupCh {
		t.Errorf("duplicate execution: %s", dup)
		dupCount++
	}

	// Count total executions.
	var totalExecutions int32
	var completedCount int
	for i := range workflows {
		totalExecutions += workflows[i].executeCount
		if workflows[i].executeCount > 0 {
			completedCount++
		}
	}

	t.Logf("No-duplicate test: %d workflows processed, %d total executions, %d duplicates detected",
		completedCount, totalExecutions, dupCount)

	// Without this, "0 duplicates" is the answer you also get when nothing
	// ran -- which is exactly what happened while the fence bug above made
	// every completion fail. A duplicate-detection test that processed no
	// workflows has detected nothing.
	if totalExecutions == 0 {
		t.Fatalf("no workflows were executed, so the duplicate count proves nothing")
	}

	if dupCount > 0 {
		t.Errorf("detected %d duplicate executions during rolling restart", dupCount)
	}
}
