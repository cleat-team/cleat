package engine

// cleat#1364. TestCascadeDelete's MySQL arms dropped the foreign keys that
// migrations/mysql/001_schema.sql ships, and passed while doing it.
//
// The asymmetry is the whole defect. On PostgreSQL and SQL Server the
// constraints that test adds are its own -- fk_test_cascade_* and
// fk_*_workflow -- and its teardown drops them BY NAME, restoring the shipped
// state. On MySQL the shipped constraints already have ON DELETE CASCADE, so
// there was nothing to add; the code dropped and re-added them anyway, and the
// teardown then searched for "any constraint referencing workflow_instances"
// and removed what it found. Measured on a database created empty: 5 before,
// 0 after, test green.
//
// WHY A SEPARATE TEST RATHER THAN TRUSTING THE FIX. The damage was invisible
// because it was done by a passing test and landed on a different one, later,
// in a shared database -- TestRetentionDeletesEveryChildRowOnPostgresAndMySQL
// failed on every full-suite run and passed in isolation, which reads as
// flakiness or a stale container rather than as destruction. A count taken
// before and after is the one observation that distinguishes them, and it is
// cheap enough to keep.

import (
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestCascadeDeleteLeavesMySQLsShippedForeignKeysAlone(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectMySQL)

	count := func(when string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
			WHERE CONSTRAINT_SCHEMA = DATABASE()
			  AND REFERENCED_TABLE_NAME = 'workflow_instances'`).Scan(&n); err != nil {
			t.Fatalf("count foreign keys %s: %v", when, err)
		}
		return n
	}

	before := count("before")
	// PRECONDITION. Five is what migrations/mysql/001_schema.sql declares, and
	// a run that starts at 0 cannot detect a drop -- it would pass for the
	// wrong reason, which is how this went unnoticed.
	if before != len(mysqlCascadeChildTables) {
		t.Fatalf("PRECONDITION FAILED: %d foreign keys to workflow_instances before the "+
			"test, want %d.\n\nThis database has already lost them. Recreate it: a run "+
			"starting at 0 cannot observe a drop and would pass for the wrong reason "+
			"(cleat#1364).", before, len(mysqlCascadeChildTables))
	}

	addCascadeFKs(t, db, testutil.DialectMySQL)
	removeCascadeFKs(t, db, testutil.DialectMySQL)

	if after := count("after"); after != before {
		t.Errorf("addCascadeFKs + removeCascadeFKs changed the foreign-key count from %d to "+
			"%d.\n\nOn MySQL these constraints are SHIPPED, not test fixtures: "+
			"migrations/mysql/001_schema.sql declares all five with ON DELETE CASCADE. "+
			"Dropping them leaves the database unable to cascade, so MySQL's retention "+
			"sweeps silently stop deleting child rows and the test that checks them fails "+
			"somewhere else entirely (cleat#1364).", before, after)
	}
}
