package upgrade

import (
	"context"
	"database/sql"
	"fmt"
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
const suiteQueue = "queue-upgrade-tests"

// testDB returns a database connection for upgrade tests.
//
// The schema comes from engine/testutil, which builds it from
// migrations/postgres/. This helper used to create every table itself with
// CREATE TABLE IF NOT EXISTS, which is how the suite came to depend on a
// workflow_instances with no foreign key to workflow_defs -- see the same note
// in tests/integrity and engine/fault_test.go.
//
// testutil.TestDB also fails, rather than skips, when CLEAT_TEST_DB is set but
// unreachable, so a database that stops arriving empties this job loudly.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)

	// worker_rolling_test.go inserts instances with def_name='test',
	// def_version=1, and workflow_instances_def_name_def_version_fkey requires
	// the definition to exist. Seeded here because nothing else in this
	// package creates it: on a machine where another suite had already made
	// the row those tests passed, and on a fresh database they failed. That is
	// an order dependency between packages, which is worth removing whether or
	// not it is currently biting.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ('test', 1, '\x00', '{}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs(test, 1): %v", err)
	}

	// Clean up leftover test data. Children first: the foreign keys apply to
	// deletes too.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'upg-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'upg-%'`)
	db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'upg-%'`)

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
		VALUES ($1, $2, 1, 'ready', '{"key":"value"}', '`+suiteQueue+`')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := engine.NewPostgresStore(db, suiteQueue)
	err = store.AppendEventHistory(ctx, runID, engine.EventRecord{
		Step: 0, EventType: engine.EventTypeCall,
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
	// input is JSONB, so comparing its ::text rendering to a Go literal
	// compares formatting, not data: PostgreSQL returns {"key": "value"},
	// with a space. All three checks in this file did that, and all three
	// failed the moment a database was pointed at them. Let PostgreSQL do the
	// comparison, and read the text only for the failure message.
	const wantInput = `{"key":"value"}`
	var input string
	var inputIntact bool
	err = db.QueryRow(`SELECT input::text, input = $2::jsonb FROM workflow_instances WHERE id = $1`,
		runID, wantInput).Scan(&input, &inputIntact)
	if err != nil {
		t.Fatalf("read instance after migration: %v", err)
	}
	if !inputIntact {
		t.Errorf("instance input changed after migration: got %s, want %s", input, wantInput)
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
		VALUES ($1, $2, 1, 'ready', '{"preserved":true}', '`+suiteQueue+`')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := engine.NewPostgresStore(db, suiteQueue)
	err = store.AppendEventHistory(ctx, runID, engine.EventRecord{
		Step: 0, EventType: engine.EventTypeCall,
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
	const wantInput = `{"preserved":true}`
	var input string
	var inputIntact bool
	err = db.QueryRow(`SELECT input::text, input = $2::jsonb FROM workflow_instances WHERE id = $1`,
		runID, wantInput).Scan(&input, &inputIntact)
	if err != nil {
		t.Fatalf("read instance after rollback: %v", err)
	}
	if !inputIntact {
		t.Errorf("instance input changed after rollback: got %s, want %s", input, wantInput)
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
		VALUES ($1, $2, 1, 'ready', '{"idempotent":true}', '`+suiteQueue+`')`, runID, defName)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := engine.NewPostgresStore(db, suiteQueue)
	err = store.AppendEventHistory(ctx, runID, engine.EventRecord{
		Step: 0, EventType: engine.EventTypeCall,
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
	const wantInput = `{"idempotent":true}`
	var input string
	var inputIntact bool
	err = db.QueryRow(`SELECT input::text, input = $2::jsonb FROM workflow_instances WHERE id = $1`,
		runID, wantInput).Scan(&input, &inputIntact)
	if err != nil {
		t.Fatalf("read instance after second migration: %v", err)
	}
	if !inputIntact {
		t.Errorf("instance input changed: got %s, want %s", input, wantInput)
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
