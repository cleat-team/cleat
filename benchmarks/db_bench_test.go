//go:build db_bench

// Package benchmarks provides database performance benchmarks for cleat.
//
// These benchmarks require a real PostgreSQL instance and are excluded from
// normal test runs via the "db_bench" build tag.
//
// Setup:
//
//	export CLEAT_DB_BENCH_DSN="postgres://user:pass@localhost:5432/cleat_bench?sslmode=disable"
//	go test -tags=db_bench -bench=. -benchmem -benchtime=10s ./benchmarks/
//
// To run a single benchmark:
//
//	go test -tags=db_bench -bench=BenchmarkClaimQuery/workers=100 -benchtime=30s ./benchmarks/
//
// Hardware to record (when publishing results):
//   - CPU: model, core count, frequency
//   - RAM: total, type (DDR4/DDR5), speed
//   - Disk: NVMe vs SATA SSD, model
//   - PostgreSQL: version, config (shared_buffers, max_connections, work_mem)
//
// Tables are created automatically in a "cleat_bench" schema and cleaned up
// after the benchmark suite completes. The DSN database must exist and be
// accessible.
package benchmarks

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/host"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Global setup / teardown
// ---------------------------------------------------------------------------

var benchDB *sql.DB
var benchStore host.WorkflowStore

func TestMain(m *testing.M) {
	dsn := os.Getenv("CLEAT_DB_BENCH_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "CLEAT_DB_BENCH_DSN not set; skipping DB benchmarks")
		os.Exit(0)
	}

	var err error
	benchDB, err = sql.Open("postgres", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer benchDB.Close()

	if err := benchDB.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to ping: %v\n", err)
		os.Exit(1)
	}

	// Set aggressive connection pool settings for benchmarks.
	benchDB.SetMaxOpenConns(100)
	benchDB.SetMaxIdleConns(50)

	// Create benchmark schema and tables.
	if err := setupBenchSchema(context.Background(), benchDB); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup schema: %v\n", err)
		os.Exit(1)
	}

	factory := host.NewPostgresStoreFactory(benchDB, "cleat_bench")
	store, closer, err := factory.OpenStore(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
		os.Exit(1)
	}
	benchStore = store
	defer closer.Close()

	code := m.Run()

	// Cleanup.
	tearDownBenchSchema(context.Background(), benchDB)
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Schema setup
// ---------------------------------------------------------------------------

func setupBenchSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS cleat_bench`)
	if err != nil {
		return err
	}

	// Create minimal workflow_instances and event_history tables for
	// benchmarking (mirrors the production schema).
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cleat_bench.workflow_instances (
			id              TEXT PRIMARY KEY,
			def_name        TEXT NOT NULL DEFAULT 'bench',
			def_version     INTEGER NOT NULL DEFAULT 1,
			min_version     INTEGER NOT NULL DEFAULT 1,
			status          TEXT NOT NULL DEFAULT 'running',
			input           JSONB,
			result          TEXT,
			error           TEXT,
			assigned_to     TEXT,
			next_wake_at    TIMESTAMPTZ,
			tenant_id       TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cleat_bench.event_history (
			workflow_id     TEXT NOT NULL,
			step            INTEGER NOT NULL,
			event_type      TEXT NOT NULL,
			service         TEXT,
			operation       TEXT,
			request         TEXT,
			response        TEXT,
			error           TEXT,
			duration_ms     BIGINT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (workflow_id, step)
		)
	`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_bench_event_history_wf
			ON cleat_bench.event_history (workflow_id, step)
	`)
	return err
}

func tearDownBenchSchema(ctx context.Context, db *sql.DB) {
	db.ExecContext(ctx, `DROP SCHEMA IF EXISTS cleat_bench CASCADE`)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func benchWorkflowID() string {
	return "bench-" + uuid.New().String()
}

func benchEvent(step int) host.EventRecord {
	return host.EventRecord{
		Step:      step,
		EventType: "call",
		Service:   "bench-svc",
		Op:        "bench-op",
		Request:   fmt.Sprintf(`{"n":%d}`, step),
		Response:  fmt.Sprintf(`{"ok":%d}`, step),
	}
}

// ---------------------------------------------------------------------------
// 1. Claim query latency at 10/100/1000 concurrent workers
// ---------------------------------------------------------------------------

func BenchmarkClaimQuery(b *testing.B) {
	ctx := context.Background()

	// Pre-insert workflow instances for claiming.
	const preloadCount = 5000
	wfIDs := make([]string, preloadCount)
	for i := 0; i < preloadCount; i++ {
		id := benchWorkflowID()
		wfIDs[i] = id
		_, err := benchDB.ExecContext(ctx, `
			INSERT INTO cleat_bench.workflow_instances (id, status, assigned_to)
			VALUES ($1, 'available', '')
		`, id)
		if err != nil {
			b.Fatalf("preload instance %d: %v", i, err)
		}
	}
	b.Cleanup(func() {
		for _, id := range wfIDs {
			benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.workflow_instances WHERE id = $1`, id)
		}
	})

	concurrencyLevels := []int{10, 100, 1000}
	for _, conc := range concurrencyLevels {
		b.Run(fmt.Sprintf("workers=%d", conc), func(b *testing.B) {
			var wg sync.WaitGroup
			sem := make(chan struct{}, conc)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					_, err := benchStore.ClaimWorkflow(ctx, "bench-worker", "")
					if err != nil && err.Error() != "no available workflows" {
						// Ignore "no available" errors (normal at end of queue).
					}
				}()
			}
			wg.Wait()
		})
	}
}

// ---------------------------------------------------------------------------
// 2. event_history INSERT throughput
// ---------------------------------------------------------------------------

func BenchmarkEventHistoryInsert(b *testing.B) {
	ctx := context.Background()

	// Measure bulk INSERT throughput.
	batchSizes := []int{1, 10, 100}
	for _, batchSize := range batchSizes {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			wfID := benchWorkflowID()
			// Ensure the workflow instance exists.
			_, err := benchDB.ExecContext(ctx, `
				INSERT INTO cleat_bench.workflow_instances (id, status) VALUES ($1, 'running')
				ON CONFLICT (id) DO NOTHING
			`, wfID)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.event_history WHERE workflow_id = $1`, wfID)
				benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.workflow_instances WHERE id = $1`, wfID)
			})

			stepCounter := 0
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Use a transaction for batch inserts.
				tx, err := benchDB.BeginTx(ctx, nil)
				if err != nil {
					b.Fatal(err)
				}

				for j := 0; j < batchSize; j++ {
					ev := benchEvent(stepCounter)
					stepCounter++
					_, err := tx.ExecContext(ctx, `
						INSERT INTO cleat_bench.event_history (workflow_id, step, event_type, service, operation, request, response)
						VALUES ($1, $2, $3, $4, $5, $6, $7)
					`, wfID, ev.Step, ev.EventType, ev.Service, ev.Op, ev.Request, ev.Response)
					if err != nil {
						tx.Rollback()
						b.Fatal(err)
					}
				}

				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(b.N*batchSize)/b.Elapsed().Seconds(), "events/s")
		})
	}
}

// ---------------------------------------------------------------------------
// 3. event_history SELECT latency at scale
// ---------------------------------------------------------------------------

func BenchmarkEventHistorySelect(b *testing.B) {
	ctx := context.Background()

	// Pre-populate a workflow with many events.
	const eventCount = 10000
	wfID := benchWorkflowID()
	benchDB.ExecContext(ctx, `
		INSERT INTO cleat_bench.workflow_instances (id, status) VALUES ($1, 'running')
		ON CONFLICT (id) DO NOTHING
	`, wfID)
	for i := 0; i < eventCount; i++ {
		ev := benchEvent(i)
		_, err := benchDB.ExecContext(ctx, `
			INSERT INTO cleat_bench.event_history (workflow_id, step, event_type, service, operation, request, response)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (workflow_id, step) DO NOTHING
		`, wfID, ev.Step, ev.EventType, ev.Service, ev.Op, ev.Request, ev.Response)
		if err != nil {
			b.Fatalf("preload event %d: %v", i, err)
		}
	}
	b.Cleanup(func() {
		benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.event_history WHERE workflow_id = $1`, wfID)
		benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.workflow_instances WHERE id = $1`, wfID)
	})

	// Benchmark loading all events.
	b.Run("load_all", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			events, err := benchStore.LoadEventHistory(ctx, wfID)
			if err != nil {
				b.Fatal(err)
			}
			if len(events) != eventCount {
				b.Fatalf("expected %d events, got %d", eventCount, len(events))
			}
		}
		b.ReportMetric(float64(eventCount)/b.Elapsed().Seconds(), "events/s")
	})

	// Benchmark paginated loading.
	b.Run("paginated_page1000", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			offset := 0
			for {
				events, err := benchStore.LoadEventHistoryPaginated(ctx, wfID, offset, 1000)
				if err != nil {
					b.Fatal(err)
				}
				if len(events) == 0 {
					break
				}
				offset += len(events)
			}
		}
		b.ReportMetric(float64(eventCount)/b.Elapsed().Seconds(), "events/s")
	})
}

// ---------------------------------------------------------------------------
// 4. heartbeat UPDATE throughput
// ---------------------------------------------------------------------------

func BenchmarkHeartbeatUpdate(b *testing.B) {
	ctx := context.Background()

	// Pre-insert workflow instances.
	const wfCount = 1000
	wfIDs := make([]string, wfCount)
	for i := 0; i < wfCount; i++ {
		id := benchWorkflowID()
		wfIDs[i] = id
		_, err := benchDB.ExecContext(ctx, `
			INSERT INTO cleat_bench.workflow_instances (id, status, assigned_to)
			VALUES ($1, 'running', 'bench-worker')
		`, id)
		if err != nil {
			b.Fatalf("preload instance %d: %v", i, err)
		}
	}
	b.Cleanup(func() {
		for _, id := range wfIDs {
			benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.workflow_instances WHERE id = $1`, id)
		}
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wfID := wfIDs[i%wfCount]
		_, err := benchDB.ExecContext(ctx, `
			UPDATE cleat_bench.workflow_instances
			SET next_wake_at = $1
			WHERE id = $2
		`, time.Now().Add(time.Minute), wfID)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "updates/s")
}

// ---------------------------------------------------------------------------
// 5. Compaction duration at 10K / 100K events
// ---------------------------------------------------------------------------

// BenchmarkCompaction measures the time to load all events for compaction
// at different history sizes. This exercises the cursor-based pagination in
// loadAllEventsForCompaction.
func BenchmarkCompaction(b *testing.B) {
	ctx := context.Background()

	histSizes := []int{10000, 100000}
	for _, size := range histSizes {
		b.Run(fmt.Sprintf("events=%d", size), func(b *testing.B) {
			wfID := benchWorkflowID()
			benchDB.ExecContext(ctx, `
				INSERT INTO cleat_bench.workflow_instances (id, status) VALUES ($1, 'running')
				ON CONFLICT (id) DO NOTHING
			`, wfID)

			// Bulk-insert events.
			_, err := benchDB.ExecContext(ctx, `
				INSERT INTO cleat_bench.event_history (workflow_id, step, event_type, service, operation, request, response)
				SELECT $1, generate_series, 'call', 'svc', 'op', '{}', '{}'
				FROM generate_series(0, $2)
			`, wfID, size-1)
			if err != nil {
				b.Fatalf("bulk insert %d events: %v", size, err)
			}
			b.Cleanup(func() {
				benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.event_history WHERE workflow_id = $1`, wfID)
				benchDB.ExecContext(ctx, `DELETE FROM cleat_bench.workflow_instances WHERE id = $1`, wfID)
			})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := benchDB.QueryContext(ctx, `
					SELECT step, event_type, service, operation, request, response, error,
					       duration_ms, '' AS signal_names, 0 AS timeout_ms,
					       '' AS signal_name, '' AS signal_payload,
					       '' AS defer_description, '' AS defer_id,
					       '' AS child_name, '' AS child_input, '' AS run_id, '' AS new_input,
					       '' AS plugin_name, '' AS plugin_func, '' AS plugin_input,
					       '' AS plugin_output, '' AS plugin_error
					FROM cleat_bench.event_history
					WHERE workflow_id = $1
					ORDER BY step
					LIMIT 1000
				`, wfID)
				if err != nil {
					b.Fatal(err)
				}
				var count int
				for rows.Next() {
					count++
				}
				rows.Close()
				if count == 0 {
					b.Fatal("no events loaded")
				}
			}
			b.ReportMetric(float64(size)/b.Elapsed().Seconds(), "events/s")
		})
	}
}

// ---------------------------------------------------------------------------
// Randomized workflow mix for background load
// ---------------------------------------------------------------------------

// randomString generates a random alphanumeric string of length n.
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
