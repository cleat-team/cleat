package scale

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// suiteQueue keeps this suite's workflows off the queues the other tests/
// suites use.
//
// Without it every DB-backed suite inserted onto "default" and constructed its
// store with no queue list, which also polls "default". Go runs distinct
// packages in parallel and they all point at CLEAT_TEST_DB, so
// `go test ./tests/integrity/... ./tests/upgrade/... ./tests/scale/...`
// had tests/scale claiming tests/integrity's workflows out from under it:
// 17 failures, and every one of them passes when the suites are run one at a
// time. ClaimWorkflows filters on `task_queue = ANY($2)`, so giving each suite
// its own queue is the whole fix. IMPROVEMENT-PLAN 2.39.
const suiteQueue = "queue-scale-tests"

// testDB returns a database connection for scale tests.
//
// The schema comes from engine/testutil, which builds it from
// migrations/postgres/. This helper used to create every table itself with
// CREATE TABLE IF NOT EXISTS -- see the same note in tests/integrity and
// tests/upgrade for what that costs.
//
// testutil.TestDB also fails, rather than skips, when CLEAT_TEST_DB is set but
// unreachable, so a database that stops arriving empties this job loudly.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)

	// Every insert site in this package uses def_name='test', def_version=1,
	// and workflow_instances_def_name_def_version_fkey requires the definition
	// to exist. Nothing else here creates it, so a fresh database would fail
	// every test in the package.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ('test', 1, '\x00', '{}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs(test, 1): %v", err)
	}

	// Clean up previous test data. Children first: the foreign keys apply to
	// deletes too.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'scale-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'scale-%'`)

	return db
}

// measureThroughput creates numWorkflows with eventsPerWF events each, using
// workerCount goroutines, and returns the total elapsed time and
// events-per-second throughput.
func measureThroughput(t *testing.T, store *engine.PostgresStore, db *sql.DB, ctx context.Context, numWorkflows, eventsPerWF, workerCount int) (time.Duration, float64) {
	t.Helper()

	// Create workflow instances.
	var wfIDs []string
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("scale-tp-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', '`+suiteQueue+`') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %d: %v", i, err)
		}
		wfIDs = append(wfIDs, id)
	}

	// Pre-build events per workflow (all identical except step).
	totalEvents := numWorkflows * eventsPerWF
	workItems := make(chan struct {
		wfID string
		step int
	}, totalEvents)
	for _, wfID := range wfIDs {
		for step := 0; step < eventsPerWF; step++ {
			workItems <- struct {
				wfID string
				step int
			}{wfID, step}
		}
	}
	close(workItems)

	var wg sync.WaitGroup
	errCh := make(chan error, workerCount)
	start := time.Now()

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range workItems {
				rec := engine.EventRecord{
					Step:      item.step,
					EventType: engine.EventTypeCall,
					Service:   "svc",
					Op:        "op",
					Request:   `{}`,
					Response:  `{"ok":true}`,
				}
				if err := store.AppendEventHistory(ctx, item.wfID, rec); err != nil {
					errCh <- fmt.Errorf("append step %d to %s: %w", item.step, item.wfID, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	elapsed := time.Since(start)

	for err := range errCh {
		t.Errorf("throughput worker error: %v", err)
	}

	throughput := float64(totalEvents) / elapsed.Seconds()

	// Cleanup.
	for _, id := range wfIDs {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
	}

	return elapsed, throughput
}

// TestThroughputSingleWorker measures event append throughput with 1 worker.
func TestThroughputSingleWorker(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	const numWorkflows = 20
	const eventsPerWF = 50

	elapsed, throughput := measureThroughput(t, store, db, ctx, numWorkflows, eventsPerWF, 1)
	totalEvents := numWorkflows * eventsPerWF

	t.Logf("Single worker throughput test:")
	t.Logf("  Workflows: %d", numWorkflows)
	t.Logf("  Events per workflow: %d", eventsPerWF)
	t.Logf("  Total events: %d", totalEvents)
	t.Logf("  Elapsed: %v", elapsed)
	t.Logf("  Throughput: %.0f events/s", throughput)

	if throughput < 10 {
		t.Errorf("throughput too low: %.0f events/s (expected > 10)", throughput)
	}
}

// TestThroughputMultiWorker measures event append throughput with N workers.
func TestThroughputMultiWorker(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	workerCounts := []int{2, 4, 8}
	const numWorkflows = 40
	const eventsPerWF = 25

	for _, n := range workerCounts {
		t.Run(fmt.Sprintf("%d workers", n), func(t *testing.T) {
			elapsed, throughput := measureThroughput(t, store, db, ctx, numWorkflows, eventsPerWF, n)
			totalEvents := numWorkflows * eventsPerWF

			t.Logf("Multi-worker throughput (%d workers):", n)
			t.Logf("  Workflows: %d", numWorkflows)
			t.Logf("  Events per workflow: %d", eventsPerWF)
			t.Logf("  Total events: %d", totalEvents)
			t.Logf("  Elapsed: %v", elapsed)
			t.Logf("  Throughput: %.0f events/s", throughput)
		})
	}
}

// TestThroughputScalingEfficiency calculates scaling efficiency across
// different worker counts.
func TestThroughputScalingEfficiency(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()

	const numWorkflows = 50
	const eventsPerWF = 20

	// Measure baseline with 1 worker.
	_, baseThroughput := measureThroughput(t, store, db, ctx, numWorkflows, eventsPerWF, 1)

	// Measure with 2, 4, 8 workers.
	workerCounts := []int{2, 4, 8}
	for _, n := range workerCounts {
		_, tp := measureThroughput(t, store, db, ctx, numWorkflows, eventsPerWF, n)
		speedup := tp / baseThroughput
		efficiency := speedup / float64(n) * 100

		t.Logf("Scaling efficiency with %d workers:", n)
		t.Logf("  Single-worker throughput: %.0f events/s", baseThroughput)
		t.Logf("  Multi-worker throughput: %.0f events/s", tp)
		t.Logf("  Speedup: %.2fx", speedup)
		t.Logf("  Efficiency: %.1f%%", efficiency)
	}
}
