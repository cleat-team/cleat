package testutil

import (
	"testing"
)

// IMPROVEMENT-PLAN 2.60d: existingTables must answer the same question the
// DELETE asks.
//
// The cleanup helpers issue `DELETE FROM <table>` unqualified, which PostgreSQL
// resolves through the whole search_path. The existence check that decides
// which tables to delete has to resolve names the same way, or the two disagree
// and cleanup silently skips tables it could perfectly well have cleared.
//
// The first version of this check used
//
//	SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()
//
// which is a different question: it asks about the *first* entry on the search
// path. Measured on a database built from the shipped migrations, with
// search_path = cleat,public -- the shape the Cluster job runs in, because
// migrations/postgres/001_schema.sql creates a `cleat` schema for
// cleat.assert_tenant_set():
//
//	current_schema()                                        -> cleat
//	information_schema.tables WHERE table_schema = ...      -> 0 rows
//	to_regclass('workflow_instances') IS NOT NULL           -> true
//
// So the check found nothing, cleanup deleted nothing, assertTablesEmpty
// verified nothing -- every one of them reporting success -- and the leftover
// rows surfaced in the Cluster job as
// `CreateSchedule: duplicate key value violates unique constraint`.
//
// That is the silent no-op this whole item exists to remove, reintroduced by
// the check meant to support it.

// TestExistingTablesResolvesThroughTheSearchPath is the regression test.
//
// Reverting existingTables to filter information_schema on current_schema()
// fails this: the count drops to zero, and with it the t.Fatalf guard fires.
func TestExistingTablesResolvesThroughTheSearchPath(t *testing.T) {
	db := SuiteTestDB(t, "testutil")

	// One connection, so the SET below applies to the one this test uses.
	// Without the cap, database/sql may hand the query to a different pooled
	// connection and the test would pass by not reproducing anything.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.SetMaxOpenConns(0) })

	if _, err := db.Exec(`SET search_path = cleat, public`); err != nil {
		t.Fatalf("setting search_path: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`SET search_path = public`) })

	// Confirm the condition is actually reproduced, rather than assuming it.
	// If current_schema() were still public this test would pass without
	// exercising the defect at all.
	var current string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("reading current_schema: %v", err)
	}
	if current == "public" {
		t.Fatalf("search_path did not take effect; current_schema() is still %q, "+
			"so this test is not reproducing the condition it exists for", current)
	}

	present := existingTables(t, db, DialectPostgres, postgresCleanupTables)
	if len(present) == 0 {
		t.Fatal("no tables resolved with search_path=cleat,public; cleanup would " +
			"delete nothing and report success")
	}

	// And the tables it found must be the ones an unqualified DELETE would hit.
	for _, tbl := range present {
		var ok bool
		if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, tbl).Scan(&ok); err != nil {
			t.Fatalf("resolving %s: %v", tbl, err)
		}
		if !ok {
			t.Errorf("existingTables returned %q, but an unqualified statement "+
				"cannot resolve it", tbl)
		}
	}
}
