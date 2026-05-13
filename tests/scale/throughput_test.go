package scale

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/internal/host"

	_ "github.com/lib/pq"
)

// testDB returns a database connection for scale tests.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping: cannot ping database: %v", err)
	}
	// Clean up previous test data.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'scale-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'scale-%'`)

	// Ensure full schema.
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_defs (
		name TEXT NOT NULL, version INTEGER NOT NULL,
		wasm_bytes BYTEA NOT NULL, entry_points TEXT[] NOT NULL DEFAULT '{}',
		min_version INTEGER NOT NULL DEFAULT 0,
		max_history_length INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (name, version))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_instances (
		id TEXT PRIMARY KEY, def_name TEXT NOT NULL, def_version INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'ready', input JSONB NOT NULL DEFAULT '{}',
		assigned_to TEXT, heartbeat_at TIMESTAMPTZ,
		next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ,
		result JSONB, error_msg TEXT, parent_workflow_id TEXT,
		trace_id TEXT,
		query_state JSONB DEFAULT '{}', task_queue TEXT NOT NULL DEFAULT 'default',
		cancellation_requested BOOLEAN NOT NULL DEFAULT false,
		cancellation_reason TEXT, sticky_worker_id TEXT)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS event_history (
		workflow_id TEXT NOT NULL, step INTEGER NOT NULL,
		event_type TEXT NOT NULL DEFAULT 'call',
		service TEXT, operation TEXT, request JSONB, response JSONB, error TEXT,
		duration_ms BIGINT, signal_names TEXT, timeout_ms BIGINT,
		signal_name TEXT, signal_payload JSONB, defer_description TEXT,
		defer_id TEXT, child_name TEXT, child_input JSONB, run_id TEXT,
		new_input JSONB, plugin_name TEXT, plugin_func TEXT,
		plugin_input JSONB, plugin_output JSONB, plugin_error TEXT,
		promise_name TEXT, promise_id TEXT, promise_result TEXT, promise_error TEXT,
		payload JSONB,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (workflow_id, step))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_signals (
		workflow_id TEXT NOT NULL, signal_name TEXT NOT NULL,
		payload JSONB NOT NULL DEFAULT '{}',
		delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (workflow_id, signal_name))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_promises (
		workflow_id TEXT NOT NULL, promise_id TEXT NOT NULL,
		promise_name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
		result JSONB, error_msg TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ,
		PRIMARY KEY (workflow_id, promise_id))`)
	db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_state JSONB`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compacted_at TIMESTAMPTZ`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_step INTEGER`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`)
	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`)
	return db
}

// measureThroughput creates numWorkflows with eventsPerWF events each, using
// workerCount goroutines, and returns the total elapsed time and
// events-per-second throughput.
func measureThroughput(t *testing.T, store *host.PostgresStore, db *sql.DB, ctx context.Context, numWorkflows, eventsPerWF, workerCount int) (time.Duration, float64) {
	t.Helper()

	// Create workflow instances.
	var wfIDs []string
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("scale-tp-%d-%d", i, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, id)
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
				rec := host.EventRecord{
					Step:      item.step,
					EventType: host.EventTypeCall,
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

	store := host.NewPostgresStore(db)
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

	store := host.NewPostgresStore(db)
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

	store := host.NewPostgresStore(db)
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
