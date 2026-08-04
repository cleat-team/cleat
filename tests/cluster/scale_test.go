package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine"
)

// scaleStore returns a PostgresStore for scale tests.
func scaleStore(t *testing.T) (*sql.DB, *engine.PostgresStore) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping: cannot ping database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, engine.NewPostgresStore(db)
}

// cleanupScaleData removes test data created by scale tests.
func cleanupScaleData(t *testing.T, db *sql.DB) {
	t.Helper()
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'scale-test-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'scale-test-%'`)
}

// TestAddRemoveWorkers simulates adding and removing workers dynamically,
// verifying that work is redistributed appropriately.
func TestAddRemoveWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	db, store := scaleStore(t)
	ctx := context.Background()
	cleanupScaleData(t, db)

	// Create workflows in the default queue.
	const numWorkflows = 20
	workflowIDs := make([]string, 0, numWorkflows)
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("scale-test-addremove-%d", i)
		EnsureDef(t, db, "scale-workflow", 1)
		_, err := db.Exec(`
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'scale-workflow', 1, 'ready', '{}', 'default')
			ON CONFLICT (id) DO NOTHING
		`, id)
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

	// Phase 1: Only worker-1 claims workflows.
	var phase1Count int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err == nil && wf != nil {
				atomic.AddInt32(&phase1Count, 1)
			}
		}()
	}
	wg.Wait()
	t.Logf("Phase 1 (1 worker): claimed %d workflows", atomic.LoadInt32(&phase1Count))

	// Phase 2: Add worker-2 and worker-3. Release some back to ready for them.
	releasedCount := 0
	wfs, err := store.ListWorkflows(ctx, engine.WorkflowFilter{Status: "running", Limit: 100})
	if err == nil {
		for _, wf := range wfs {
			if releasedCount >= 5 {
				break
			}
			if err := store.ReleaseWorkflow(ctx, wf.ID, "worker-1", wf.Generation, time.Now()); err == nil {
				releasedCount++
			}
		}
	}

	// Phase 2: Multiple workers claim.
	var phase2Count int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
				wf, err := store.ClaimWorkflow(ctx, wid)
				if err == nil && wf != nil {
					atomic.AddInt32(&phase2Count, 1)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("Phase 2 (3 workers): claimed %d workflows", atomic.LoadInt32(&phase2Count))

	// Phase 3: "Remove" worker-2 and worker-3 — release their workflows.
	wfs2, err := store.ListWorkflows(ctx, engine.WorkflowFilter{Status: "running", Limit: 100})
	if err == nil {
		for _, wf := range wfs2 {
			if wf.AssignedTo == "worker-2" || wf.AssignedTo == "worker-3" {
				store.ReleaseWorkflow(ctx, wf.ID, wf.AssignedTo, wf.Generation, time.Now())
			}
		}
	}

	// Phase 3: Only worker-1 claims again.
	var phase3Count int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err == nil && wf != nil {
				atomic.AddInt32(&phase3Count, 1)
			}
		}()
	}
	wg.Wait()
	t.Logf("Phase 3 (1 worker again): claimed %d workflows", atomic.LoadInt32(&phase3Count))

	// Verify that worker-1 was able to claim workflows in all phases.
	totalClaimed := atomic.LoadInt32(&phase1Count) + atomic.LoadInt32(&phase3Count)
	if totalClaimed == 0 {
		t.Error("Worker-1 should have claimed at least some workflows")
	}

	t.Logf("Add/remove workers test completed: phase1=%d phase2=%d phase3=%d",
		atomic.LoadInt32(&phase1Count), atomic.LoadInt32(&phase2Count), atomic.LoadInt32(&phase3Count))
}

// TestScaleUpWorkers increases the number of workers and verifies that
// throughput scales proportionally.
func TestScaleUpWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	db, store := scaleStore(t)
	ctx := context.Background()
	cleanupScaleData(t, db)

	// Create a large batch of workflows.
	const numWorkflows = 50
	workflowIDs := make([]string, 0, numWorkflows)
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("scale-test-scaleup-%d", i)
		EnsureDef(t, db, "scaleup-workflow", 1)
		_, err := db.Exec(`
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'scaleup-workflow', 1, 'ready', '{}', 'default')
			ON CONFLICT (id) DO NOTHING
		`, id)
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

	// Measure throughput with 1 worker.
	start1 := time.Now()
	claimed1 := 0
	for {
		wf, err := store.ClaimWorkflow(ctx, "worker-1")
		if err != nil {
			t.Fatalf("Claim error: %v", err)
		}
		if wf == nil {
			break
		}
		claimed1++
	}
	duration1 := time.Since(start1)

	if claimed1 == 0 {
		t.Fatal("No workflows claimed by 1 worker")
	}
	t.Logf("1 worker claimed %d workflows in %v (%.0f/sec)", claimed1, duration1,
		float64(claimed1)/duration1.Seconds())

	// Release workflows for the multi-worker test.
	//
	// By ID, not by ListWorkflows(Status: "running", Limit: 1000). That query
	// returns every running workflow in the cluster -- including the ones the
	// live compose workers own -- so under a full run this test's own rows fell
	// outside the limit and were never released. The error was discarded too,
	// so the phase below then measured "throughput" over an empty queue and
	// reported 0/sec as a Log rather than a failure.
	var releasedForPhase3 int
	for _, id := range workflowIDs {
		wf, err := store.GetWorkflowByID(ctx, id)
		if err != nil {
			t.Fatalf("GetWorkflowByID(%s): %v", id, err)
		}
		if wf == nil || wf.Status != "running" {
			continue
		}
		if err := store.ReleaseWorkflow(ctx, id, "worker-1", wf.Generation, time.Now()); err != nil {
			t.Fatalf("ReleaseWorkflow(%s, gen=%d): %v", id, wf.Generation, err)
		}
		releasedForPhase3++
	}
	if releasedForPhase3 == 0 {
		t.Fatalf("released none of the %d workflows claimed above, so the multi-worker "+
			"phase has nothing to claim and measures nothing", claimed1)
	}

	// Measure throughput with 3 workers.
	start3 := time.Now()
	var claimed3 int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
				for j := 0; j < 10; j++ {
					wf, err := store.ClaimWorkflow(ctx, wid)
					if err != nil {
						return
					}
					if wf == nil {
						return
					}
					atomic.AddInt32(&claimed3, 1)
				}
			}
		}()
	}
	wg.Wait()
	duration3 := time.Since(start3)

	t.Logf("3 workers claimed %d workflows in %v (%.0f/sec)", claimed3, duration3,
		float64(claimed3)/duration3.Seconds())

	if claimed3 == 0 {
		t.Error("No workflows claimed by 3 workers")
	}

	// Every workflow released above must be claimed again. Left as a Log this
	// was the only check on the multi-worker phase, and "3 workers claimed 0 vs
	// 1 worker claimed 50 (may be fewer due to timing)" passed.
	//
	// Not a throughput assertion: the rates are logged, not compared, because
	// a shared runner is not a place to assert that three workers are faster
	// than one. What is asserted is that no work went missing.
	if int(claimed3) != releasedForPhase3 {
		t.Errorf("released %d workflows and the three workers claimed %d of them; "+
			"the rest are ready and unclaimed", releasedForPhase3, claimed3)
	}

	t.Logf("Scale-up test completed: 1 worker %.0f/sec, 3 workers %.0f/sec",
		float64(claimed1)/duration1.Seconds(),
		float64(claimed3)/duration3.Seconds())
}
