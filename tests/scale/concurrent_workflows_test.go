package scale

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/engine"
)

// numConcurrentWorkflows returns the number of workflows to create for
// concurrency tests. Can be overridden via environment variable.
func numConcurrentWorkflows() int {
	const defaultN = 1000
	return defaultN
}

// TestMaxConcurrentWorkflows creates many workflow instances and verifies all
// complete successfully.
func TestMaxConcurrentWorkflows(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	n := numConcurrentWorkflows()
	t.Logf("Creating %d concurrent workflows...", n)

	// Create all workflow instances in the DB.
	var wfIDs []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("scale-maxwf-%d-%d", i, time.Now().UnixNano())
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

	// Simulate concurrent claim + append + completion using multiple workers.
	const numWorkers = 20
	workCh := make(chan string, n)
	for _, id := range wfIDs {
		workCh <- id
	}
	close(workCh)

	var wg sync.WaitGroup
	errCh := make(chan error, n)

	start := time.Now()

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			for wfID := range workCh {
				// Claim the workflow.
				wf, err := store.ClaimWorkflow(ctx, workerID)
				if err != nil {
					errCh <- fmt.Errorf("claim %s: %w", wfID, err)
					continue
				}
				if wf == nil {
					continue
				}

				// Append a few events.
				events := []engine.EventRecord{
					{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{"ok":true}`},
					{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "process", Request: `{}`, Response: `{"ok":true}`},
				}
				if err := store.AppendEventHistoryBatch(ctx, wfID, events); err != nil {
					errCh <- fmt.Errorf("append events to %s: %w", wfID, err)
					continue
				}

				// Complete the workflow.
				if err := store.CompleteWorkflow(ctx, wfID, workerID, 0, `{"status":"success"}`, nil); err != nil {
					errCh <- fmt.Errorf("complete %s: %w", wfID, err)
				}
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()
	close(errCh)

	elapsed := time.Since(start)

	var errCount int
	for err := range errCh {
		t.Errorf("concurrent workflow error: %v", err)
		errCount++
	}

	// Verify all workflows reached a terminal state.
	var completed, running, other int
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
		switch wf.Status {
		case "done":
			completed++
		case "running", "ready":
			running++
		default:
			other++
		}
	}

	t.Logf("Concurrent workflow test results:")
	t.Logf("  Total workflows: %d", n)
	t.Logf("  Workers: %d", numWorkers)
	t.Logf("  Elapsed: %v", elapsed)
	t.Logf("  Throughput: %.0f workflows/s", float64(n)/elapsed.Seconds())
	t.Logf("  Completed: %d", completed)
	t.Logf("  Running/Ready: %d", running)
	t.Logf("  Other: %d", other)
	t.Logf("  Errors: %d", errCount)

	if completed < n-errCount {
		t.Errorf("expected ~%d completed workflows, got %d", n-errCount, completed)
	}
}

// TestConcurrentWorkflowMemory measures memory usage when many concurrent
// workflows are being processed.
func TestConcurrentWorkflowMemory(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	n := numConcurrentWorkflows()

	// Measure baseline memory.
	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	// Create workflows and process them concurrently.
	var wfIDs []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("scale-mem-%d-%d", i, time.Now().UnixNano())
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

	// Append events for each workflow.
	for _, id := range wfIDs {
		rec := engine.EventRecord{
			Step:      0,
			EventType: engine.EventTypeCall,
			Service:   "svc",
			Op:        "op",
			Request:   `{}`,
			Response:  `{"ok":true}`,
		}
		if err := store.AppendEventHistory(ctx, id, rec); err != nil {
			t.Errorf("append event to %s: %v", id, err)
		}
	}

	// Measure memory after creating all workflows.
	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	allocMB := float64(m1.TotalAlloc-m0.TotalAlloc) / 1024 / 1024
	heapMB := float64(m1.HeapInuse-m0.HeapInuse) / 1024 / 1024

	t.Logf("Memory usage for %d concurrent workflows:", n)
	t.Logf("  Baseline Alloc: %.1f MB", float64(m0.TotalAlloc)/1024/1024)
	t.Logf("  After Alloc: %.1f MB", float64(m1.TotalAlloc)/1024/1024)
	t.Logf("  Delta Alloc: %.1f MB", allocMB)
	t.Logf("  Delta HeapInuse: %.1f MB", heapMB)
	t.Logf("  Allocation per workflow: %.1f KB", allocMB*1024/float64(n))
}
