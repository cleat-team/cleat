package engine

// cleat#3245 Phase 2 (combined #3272 + #3274, owner decision 2026-10-09: "one
// combined PR"). Asserts the schema facts migration 015 is supposed to
// produce -- not the HOT ratio or WAL cost themselves, which were measured
// manually on a synthetic population (see cleat#3272's design-note comment)
// and are not re-asserted here: a HOT ratio depends on pg_stat_user_tables,
// a counter shared by every test in this package against one database, and
// would be flaky by construction (CLAUDE.md's own warning against exactly
// this shape of assertion). What IS deterministic, and what this guards, is
// the DDL itself: dropping heartbeat_at from both indexes is a necessary
// precondition for any HOT benefit, so a regression here (someone restoring
// the column, e.g. while "fixing" a reaper-query regression without
// re-deriving the tradeoff) silently loses the fix this migration exists
// for.
//
// Every query below resolves "workflow_instances" (and the two index names)
// via a ::regclass cast rather than comparing current_schema() as text --
// this test database puts the table in "public" while a fresh session's own
// current_schema() resolves to "cleat" (a schema also named after the
// connecting role, ahead of "public" in the default search_path), so a
// current_schema()-based filter found zero rows the first time this was
// written, against a table and both indexes that genuinely exist. ::regclass
// performs the same search_path-based resolution any unqualified reference
// in the application's own SQL already relies on, so it cannot have that
// mismatch by construction.

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestIdxInstancesHeartbeatDoesNotIndexHeartbeatAt(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	var def string
	if err := db.QueryRow(
		`SELECT pg_get_indexdef('idx_instances_heartbeat'::regclass)`,
	).Scan(&def); err != nil {
		t.Fatalf("idx_instances_heartbeat: %v", err)
	}
	if strings.Contains(def, "heartbeat_at") {
		t.Errorf("idx_instances_heartbeat still indexes heartbeat_at (%s) -- this is half of what makes "+
			"a heartbeat UPDATE eligible to be HOT (see migration 015); restoring it silently loses "+
			"the fix cleat#3272/#3274 measured", def)
	}

	// Negative control: the column must still be a real column on the table,
	// or this test would pass vacuously against a renamed/dropped column
	// rather than against the index definition it claims to check.
	if _, err := db.Query(`SELECT heartbeat_at FROM workflow_instances LIMIT 0`); err != nil {
		t.Fatalf("control failed: workflow_instances.heartbeat_at does not exist at all (%v) -- "+
			"this test's premise (a column present on the table but absent from the index) does not hold", err)
	}
}

func TestIdxInstancesStaleDoesNotIndexHeartbeatAt(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	var def string
	if err := db.QueryRow(
		`SELECT pg_get_indexdef('idx_instances_stale'::regclass)`,
	).Scan(&def); err != nil {
		t.Fatalf("idx_instances_stale: %v", err)
	}
	if strings.Contains(def, "heartbeat_at") {
		t.Errorf("idx_instances_stale still indexes heartbeat_at (%s) -- this is the other half of "+
			"what makes a heartbeat UPDATE eligible to be HOT (see migration 015); restoring it "+
			"silently loses the fix cleat#3272/#3274 measured, and ALSO changes ReapStaleInstances/"+
			"ListStaleHolders back to an index range scan instead of the accepted heap-scan-plus-sort "+
			"tradeoff", def)
	}
}

func TestWorkflowInstancesFillfactorIsLoweredForHotUpdates(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	var optsCSV string
	if err := db.QueryRow(
		`SELECT coalesce(array_to_string(reloptions, ','), '') FROM pg_class WHERE oid = 'workflow_instances'::regclass`,
	).Scan(&optsCSV); err != nil {
		t.Fatalf("workflow_instances reloptions: %v", err)
	}
	found := false
	for _, o := range strings.Split(optsCSV, ",") {
		if strings.HasPrefix(o, "fillfactor=") {
			found = true
			if o == "fillfactor=100" {
				t.Errorf("workflow_instances fillfactor is still the default (100) -- migration 015's "+
					"whole point is reserving page space for HOT updates; got %q", o)
			}
		}
	}
	if !found {
		t.Error("workflow_instances has no fillfactor reloption set at all -- migration 015 should have set one")
	}
}
