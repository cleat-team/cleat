package testutil

import (
	"testing"
)

// IMPROVEMENT-PLAN 2.60d: a cleanup that silently did nothing was
// indistinguishable from one that worked.
//
// The deletes are now t.Fatalf rather than t.Logf, but an error was never the
// failure that cost the most. A DELETE issued on a connection whose rows are
// hidden from it removes nothing and reports no error -- PostgreSQL RLS filters
// it to the caller's tenant, and SQL Server applies its security policy to
// every principal including sysadmin. §3.37 is exactly that: CleanupMSSQLTestData
// deleted nothing, reported success, and rows accumulated until an unrelated
// fixture collided on a primary key, which is §2.71's 141-failure signature.
//
// So the check that matters is "is the table empty now", not "did the statement
// return an error". These tests prove that check sees rows a cleanup missed.

// TestNonEmptyTablesSeesRowsCleanupMissed is the one that would have caught
// §3.37. It inserts a row, does *not* delete it, and asserts the verification
// reports it -- simulating a delete that was filtered away silently.
func TestNonEmptyTablesSeesRowsCleanupMissed(t *testing.T) {
	db := SuiteTestDB(t, "testutil")
	SetupMinimalSchema(t, db, DialectPostgres)
	CleanupPostgresTestData(t, db)

	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		 VALUES ('cleanup-verify-fixture', 1, '\x00', '00000000-0000-0000-0000-000000000000')`,
	); err != nil {
		t.Fatalf("seeding a row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name = 'cleanup-verify-fixture'`)
	})

	identity := func(s string) string { return s }

	leftover, err := nonEmptyTables(db, []string{"workflow_defs"}, identity)
	if err != nil {
		t.Fatalf("nonEmptyTables: %v", err)
	}
	if len(leftover) == 0 {
		t.Fatal("a table with a row in it was reported as empty; the verification " +
			"cannot see rows that cleanup failed to remove, which is the whole " +
			"point of it")
	}

	// Control: the same call against a table that really is empty must stay
	// quiet. Without this, a verification that always reported "not empty"
	// would pass the assertion above and fail every cleanup in the tree.
	empty, err := nonEmptyTables(db, []string{"workflow_signals"}, identity)
	if err != nil {
		t.Fatalf("nonEmptyTables on an empty table: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty table was reported as non-empty: %v", empty)
	}
}

// TestNonEmptyTablesReportsEveryOffender pins that the report names all the
// tables, not just the first. Cleanup runs on the order of a hundred times a
// suite; a message naming one table when three are dirty sends the reader back
// for another run to find the rest.
func TestNonEmptyTablesReportsEveryOffender(t *testing.T) {
	db := SuiteTestDB(t, "testutil")
	SetupMinimalSchema(t, db, DialectPostgres)
	CleanupPostgresTestData(t, db)

	const tenant = "00000000-0000-0000-0000-000000000000"
	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		 VALUES ('cleanup-multi-fixture', 1, '\x00', $1)`, tenant); err != nil {
		t.Fatalf("seeding workflow_defs: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id)
		 VALUES ('cleanup-multi-wf', 'cleanup-multi-fixture', 1, 'ready', '{}', $1)`,
		tenant); err != nil {
		t.Fatalf("seeding workflow_instances: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = 'cleanup-multi-wf'`)
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name = 'cleanup-multi-fixture'`)
	})

	leftover, err := nonEmptyTables(db,
		[]string{"workflow_instances", "workflow_defs"},
		func(s string) string { return s })
	if err != nil {
		t.Fatalf("nonEmptyTables: %v", err)
	}
	if len(leftover) != 2 {
		t.Errorf("reported %d non-empty tables, want 2: %v", len(leftover), leftover)
	}
}
