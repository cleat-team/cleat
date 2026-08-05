package migration

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestRunner_AppliesShippedMySQLMigrations is the MySQL twin of
// TestRunner_AppliesShippedPostgresMigrations, and it exists because nothing in
// the repo had ever run the Runner over migrations/mysql/.
//
// Every cleat-worker executes this Runner at boot (cmd/cleat-worker/main.go)
// and calls os.Exit(1) when it returns an error, so a failure here is not a
// test-harness problem: it is a worker that cannot start against a MySQL
// database whose schema has not been built by some other means. That is what
// this found. IMPROVEMENT-PLAN 3.13.
//
// Two independent reasons the shipped files could not be applied, both in how
// the Runner cuts a file into statements rather than in the SQL itself:
//
//	001_schema.sql: splitSQL split on every ';', including one inside the
//	comment "-- CREATE INDEX has no IF NOT EXISTS in MySQL 8.0; re-runs error
//	harmlessly.", so the runner sent `re-runs error harmlessly.` to the server:
//	   Error 1064 (42000): You have an error in your SQL syntax ...
//	   near 're-runs error harmlessly.
//
//	003_procedures.sql: DELIMITER is a client directive, not a server
//	statement, and the procedure bodies are full of semicolons. Splitting on
//	';' cuts each CREATE PROCEDURE into fragments and then sends `DELIMITER //`
//	to a server that has never heard of it.
//
// The MySQL test paths in this repo all build their schema from
// engine/testutil's Go copy instead, which is why neither showed up: the
// shipped schema was not the tested schema on MySQL (IMPROVEMENT-PLAN 1.9).
func TestRunner_AppliesShippedMySQLMigrations(t *testing.T) {
	db := newMySQLScratchDB(t, "cleat_migration_mysql_bootstrap_test")
	ctx := context.Background()

	r := NewRunner(db, engine.DialectMySQL, migrationsRoot(t))
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run against the shipped MySQL migrations failed: %v\n\n"+
			"This is the code path every cleat-worker takes at boot. If it "+
			"fails, no worker can start against a MySQL deployment whose "+
			"schema was not built by hand.", err)
	}

	// The engine's hard requirement: the tables its SQL names have to exist.
	// Checked by name rather than by counting, so a partially-applied file
	// reads as the specific thing that is missing.
	for _, table := range []string{
		"workflow_defs",
		"workflow_instances",
		"event_history",
		"idempotency_keys",
		"schema_migrations",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.TABLES
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("%s does not exist after the migrations ran", table)
		}
	}

	// finalize_workflow_status is the one the engine calls on every workflow
	// completion, with no fallback -- 003_procedures.sql exists to create it,
	// and it is the file the DELIMITER handling is for.
	var procs int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.ROUTINES
		 WHERE ROUTINE_SCHEMA = DATABASE() AND ROUTINE_NAME = 'finalize_workflow_status'`).Scan(&procs); err != nil {
		t.Fatalf("look up finalize_workflow_status: %v", err)
	}
	if procs != 1 {
		t.Errorf("finalize_workflow_status does not exist after the migrations ran: "+
			"the engine calls it on every workflow completion and has no fallback (count=%d)", procs)
	}

	// 010 is the one whose effect is a column rather than a table, and it is
	// the reason this test's sibling in idempotency_tenant_test.go had to skip.
	var pkColumns int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'idempotency_keys'
		   AND INDEX_NAME = 'PRIMARY'`).Scan(&pkColumns); err != nil {
		t.Fatalf("look up idempotency_keys primary key: %v", err)
	}
	if pkColumns != 2 {
		t.Errorf("idempotency_keys' primary key has %d column(s), want 2 "+
			"(key_hash, tenant_id) -- migration 010 did not take effect", pkColumns)
	}
}

// TestRunner_SecondMySQLRunAppliesNothing is the operator-upgrade path: a
// worker restarting against an already-migrated MySQL database must not
// re-apply anything.
//
// Worth its own test rather than folded into the one above, because the two
// failure modes are different: applying nothing on a fresh database is a
// worker that cannot start, and re-applying on a live one is DDL running under
// traffic.
func TestRunner_SecondMySQLRunAppliesNothing(t *testing.T) {
	db := newMySQLScratchDB(t, "cleat_migration_mysql_twice_test")
	ctx := context.Background()
	root := migrationsRoot(t)

	if err := NewRunner(db, engine.DialectMySQL, root).Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	var firstApplied string
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(MAX(applied_at) AS CHAR) FROM schema_migrations`).Scan(&firstApplied); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}

	if err := NewRunner(db, engine.DialectMySQL, root).Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	var secondApplied string
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(MAX(applied_at) AS CHAR) FROM schema_migrations`).Scan(&secondApplied); err != nil {
		t.Fatalf("read applied_at after second run: %v", err)
	}
	if firstApplied != secondApplied {
		t.Errorf("second Run re-applied migrations: applied_at moved from %s to %s",
			firstApplied, secondApplied)
	}
}
