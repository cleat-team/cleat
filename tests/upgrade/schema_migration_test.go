package upgrade

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	host "github.com/rcownie/cleat/internal/host"

	_ "github.com/lib/pq"
)

// testDB returns a database connection for upgrade tests.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping upgrade test in short mode")
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
	// Clean up leftover test data.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'upg-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'upg-%'`)
	db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'upg-%'`)

	// Ensure base schema exists.
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_defs (
		name TEXT NOT NULL, version INTEGER NOT NULL,
		wasm_bytes BYTEA NOT NULL, entry_points TEXT[] NOT NULL DEFAULT '{}',
		min_version INTEGER NOT NULL DEFAULT 0, namespace TEXT NOT NULL DEFAULT 'default',
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
		namespace TEXT NOT NULL DEFAULT 'default', trace_id TEXT,
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
	db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`)
	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`)
	return db
}

// TestMigrationNoDataLoss applies a schema migration and verifies existing data
// is intact after the migration.
func TestMigrationNoDataLoss(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	ctx := context.Background()

	// Insert existing data before migration.
	defName := fmt.Sprintf("upg-mig-noloss-%d", time.Now().UnixNano())
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, 1, $2, '{test}') ON CONFLICT DO NOTHING`, defName, wasmBytes)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	runID := fmt.Sprintf("upg-mig-noloss-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, $2, 1, 'ready', '{"key":"value"}', 'default')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := host.NewPostgresStore(db)
	err = store.AppendEventHistory(ctx, runID, host.EventRecord{
		Step: 0, EventType: host.EventTypeCall,
		Service: "svc", Op: "op", Request: `{"original":"data"}`, Response: `{"ok":true}`,
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}

	// Apply a schema migration: add a new column.
	migrationQueries := []string{
		`ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS mig_test_col TEXT`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS mig_test_col TEXT`,
		`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS mig_test_col TEXT`,
	}
	for _, q := range migrationQueries {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("migration query %q: %v", q, err)
		}
	}

	// Verify existing data is intact after migration.
	var input string
	err = db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&input)
	if err != nil {
		t.Fatalf("read instance after migration: %v", err)
	}
	if input != `{"key":"value"}` {
		t.Errorf("instance input changed after migration: got %q", input)
	}

	// Verify event history is intact.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory after migration: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event after migration, got %d", len(history))
	}
	if history[0].Request != `{"original":"data"}` {
		t.Errorf("event request changed after migration: got %q", history[0].Request)
	}

	// Verify we can write to the new column and read it back.
	_, err = db.Exec(`UPDATE workflow_instances SET mig_test_col = 'migrated' WHERE id = $1`, runID)
	if err != nil {
		t.Fatalf("update new column: %v", err)
	}
	var colVal string
	err = db.QueryRow(`SELECT mig_test_col FROM workflow_instances WHERE id = $1`, runID).Scan(&colVal)
	if err != nil {
		t.Fatalf("read new column: %v", err)
	}
	if colVal != "migrated" {
		t.Errorf("new column value mismatch: got %q", colVal)
	}

	t.Log("Migration completed with no data loss")
}

// TestMigrationRollback verifies that a schema migration can be rolled back
// without losing existing data.
func TestMigrationRollback(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	ctx := context.Background()

	// Insert data before migration.
	defName := fmt.Sprintf("upg-mig-roll-%d", time.Now().UnixNano())
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, 1, $2, '{test}') ON CONFLICT DO NOTHING`, defName, wasmBytes)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	runID := fmt.Sprintf("upg-mig-roll-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, $2, 1, 'ready', '{"preserved":true}', 'default')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := host.NewPostgresStore(db)
	err = store.AppendEventHistory(ctx, runID, host.EventRecord{
		Step: 0, EventType: host.EventTypeCall,
		Service: "svc", Op: "op", Request: `{"preserved":true}`, Response: `{"ok":true}`,
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}

	// Apply migration: add a column.
	_, err = db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS rollback_col TEXT`)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	// Write data to the new column.
	_, err = db.Exec(`UPDATE workflow_instances SET rollback_col = 'temp_data' WHERE id = $1`, runID)
	if err != nil {
		t.Fatalf("write to new column: %v", err)
	}

	// Rollback: drop the column.
	_, err = db.Exec(`ALTER TABLE workflow_instances DROP COLUMN IF EXISTS rollback_col`)
	if err != nil {
		t.Fatalf("rollback migration: %v", err)
	}

	// Verify original data is intact after rollback.
	var input string
	err = db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&input)
	if err != nil {
		t.Fatalf("read instance after rollback: %v", err)
	}
	if input != `{"preserved":true}` {
		t.Errorf("instance input changed after rollback: got %q", input)
	}

	// Verify event history is intact.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory after rollback: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event after rollback, got %d", len(history))
	}
	if history[0].Request != `{"preserved":true}` {
		t.Errorf("event request changed after rollback: got %q", history[0].Request)
	}
}

// TestMigrationIdempotent verifies that applying a migration twice produces
// the same result (no errors, no double-effects).
func TestMigrationIdempotent(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	ctx := context.Background()

	// Insert data.
	defName := fmt.Sprintf("upg-mig-idem-%d", time.Now().UnixNano())
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, 1, $2, '{test}') ON CONFLICT DO NOTHING`, defName, wasmBytes)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	runID := fmt.Sprintf("upg-mig-idem-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, $2, 1, 'ready', '{"idempotent":true}', 'default')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := host.NewPostgresStore(db)
	err = store.AppendEventHistory(ctx, runID, host.EventRecord{
		Step: 0, EventType: host.EventTypeCall,
		Service: "svc", Op: "op", Request: `{"idempotent":true}`, Response: `{"ok":true}`,
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}

	// Apply migration the first time.
	migration := `ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS idempotent_col TEXT`
	if _, err := db.Exec(migration); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// Write data.
	_, err = db.Exec(`UPDATE workflow_instances SET idempotent_col = 'first' WHERE id = $1`, runID)
	if err != nil {
		t.Fatalf("write after first migration: %v", err)
	}

	var colVal string
	err = db.QueryRow(`SELECT idempotent_col FROM workflow_instances WHERE id = $1`, runID).Scan(&colVal)
	if err != nil {
		t.Fatalf("read after first migration: %v", err)
	}
	if colVal != "first" {
		t.Errorf("expected 'first', got %q", colVal)
	}

	// Apply the same migration a second time — should be a no-op.
	if _, err := db.Exec(migration); err != nil {
		t.Fatalf("second migration: %v", err)
	}

	// Verify data is still intact and unchanged.
	err = db.QueryRow(`SELECT idempotent_col FROM workflow_instances WHERE id = $1`, runID).Scan(&colVal)
	if err != nil {
		t.Fatalf("read after second migration: %v", err)
	}
	if colVal != "first" {
		t.Errorf("data changed after second migration: expected 'first', got %q", colVal)
	}

	// Verify original data is intact.
	var input string
	err = db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&input)
	if err != nil {
		t.Fatalf("read instance after second migration: %v", err)
	}
	if input != `{"idempotent":true}` {
		t.Errorf("instance input changed: got %q", input)
	}

	// Apply multiple migration-style ALTER TABLE statements that match the
	// project's migration patterns — all should be idempotent.
	idempotentMigrations := []string{
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS idempotent_col TEXT`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`,
		`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_state JSONB`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compacted_at TIMESTAMPTZ`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_step INTEGER`,
	}
	for _, q := range idempotentMigrations {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("idempotent migration %q failed: %v", q, err)
		}
	}
}
