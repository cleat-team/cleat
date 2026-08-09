package migration

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestShippedMySQLSchemaAcceptsEventsWithEmptyCallColumns is the test that was
// missing, and the shape of the gap matters more than the bug it caught.
//
// migration/mysql_bootstrap_test.go already proves the shipped MySQL migrations
// APPLY. Nothing proved the engine WORKS against the schema they produce, and
// the two are different questions: 001_schema.sql declared
// event_history.service/operation/request NOT NULL, while the engine writes all
// three through nullStr (engine/store_events.go), which maps "" to SQL NULL.
// Event types that legitimately leave them empty -- EventTypeDefer,
// EventTypeChildWorkflow, EventTypeAwaitSignals, EventTypeUpdateHandler -- had
// therefore always been handing NULL to a NOT NULL column on MySQL.
//
// Every MySQL test in the repo missed it because engine/testutil/mysql_schema.go
// builds those columns nullable. The tested schema was not the shipped schema,
// so the divergence was invisible from inside the suite by construction. This
// test exists at the one place that can see it: a database built by the real
// Runner over migrations/mysql/.
//
// Why the bug was latent rather than fatal, which is worth recording because
// the first diagnosis got it wrong: the main write path is INSERT IGNORE, and
// MySQL downgrades error 1048 under IGNORE to a warning and substitutes the
// implicit default -- the empty string, which is the value the Go side started
// from. It round-trips, the checksum chain still matches, and nothing fails.
// The path that does NOT survive is WriteCallIntent (engine/store_intent.go),
// a plain INSERT with no IGNORE, covered by the second subtest below.
//
// To watch this fail, drop migrations/mysql/030 and re-run: the intent subtest
// reports Error 1048 (23000): Column 'request' cannot be null.
func TestShippedMySQLSchemaAcceptsEventsWithEmptyCallColumns(t *testing.T) {
	db := newMySQLScratchDB(t, "cleat_migration_mysql_event_columns_test")
	ctx := context.Background()

	if err := NewRunner(db, engine.DialectMySQL, migrationsRoot(t)).Run(ctx); err != nil {
		t.Fatalf("apply shipped MySQL migrations: %v", err)
	}

	// Assert the post-migration shape directly rather than inferring it from a
	// successful INSERT. INSERT IGNORE would report success against either
	// shape, so an insert-only assertion here could not distinguish "the column
	// is nullable" from "MySQL silently coerced my NULL to an empty string" --
	// which is exactly the ambiguity that hid this for as long as it did.
	t.Run("call columns are nullable", func(t *testing.T) {
		for _, col := range []string{"service", "operation", "request"} {
			var nullable string
			if err := db.QueryRowContext(ctx,
				`SELECT IS_NULLABLE FROM information_schema.COLUMNS
				 WHERE TABLE_SCHEMA = DATABASE()
				   AND TABLE_NAME = 'event_history' AND COLUMN_NAME = ?`, col).Scan(&nullable); err != nil {
				t.Fatalf("look up event_history.%s: %v", col, err)
			}
			if nullable != "YES" {
				t.Errorf("event_history.%s is IS_NULLABLE=%q, want YES.\n"+
					"PostgreSQL declares this column TEXT and SQL Server declares it NULL. "+
					"The engine writes NULL here for defer, child-workflow, await-signals "+
					"and update-handler events, so a NOT NULL constraint makes MySQL the "+
					"one dialect that rejects writes the engine actually performs.", col, nullable)
			}
		}
	})

	// The path with no INSERT IGNORE behind it. This is the subtest that goes
	// red without migration 030 -- and it is the reason the fix is a migration
	// rather than a note saying the coercion is harmless.
	t.Run("plain INSERT accepts a NULL request", func(t *testing.T) {
		// The FK chain is workflow_defs -> workflow_instances -> event_history,
		// so both ancestors have to exist. Seeding them is not incidental
		// setup: without it the INSERT still fails, but with error 1452 rather
		// than 1048 -- a red for the wrong reason, which would have let this
		// test "prove" the fix while measuring a foreign key.
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES (?, ?, ?)
		`, "test_wf", 1, []byte{0x00, 0x61, 0x73, 0x6d}); err != nil {
			t.Fatalf("seed parent workflow_defs row: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version) VALUES (?, ?, ?)
		`, "wf-intent-empty-request", "test_wf", 1); err != nil {
			t.Fatalf("seed parent workflow_instances row: %v", err)
		}

		_, err := db.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
				created_at, intent_at, tenant_id)
			VALUES (?, ?, ?, NULL, NULL, NULL, NOW(6), NOW(6), ?)
		`, "wf-intent-empty-request", 0, "call", "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("plain INSERT with NULL call columns failed: %v\n\n"+
				"This mirrors MySQLStore.WriteCallIntent (engine/store_intent.go), which "+
				"has no INSERT IGNORE to absorb the constraint. A WriteAheadIntent "+
				"operation with an empty request body fails here on MySQL and succeeds "+
				"on PostgreSQL and SQL Server.", err)
		}
	})

	// Guard the reason this was invisible, not just the symptom. If a future
	// change reintroduces NOT NULL, the subtest above catches it; if a future
	// change makes the columns nullable in testutil only, nothing here fires --
	// which is why Stream A (build every test database from migrations) is the
	// change that actually closes the class.
	t.Run("event_history exists with the columns the engine names", func(t *testing.T) {
		var cols string
		if err := db.QueryRowContext(ctx,
			`SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY COLUMN_NAME)
			 FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'event_history'`).Scan(&cols); err != nil {
			t.Fatalf("list event_history columns: %v", err)
		}
		for _, want := range []string{"defer_id", "child_name", "signal_names", "intent_at", "checksum"} {
			if !strings.Contains(cols, want) {
				t.Errorf("event_history has no %s column after the shipped migrations ran", want)
			}
		}
	})
}
