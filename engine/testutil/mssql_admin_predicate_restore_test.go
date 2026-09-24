package testutil

import (
	"os"
	"testing"
)

// cleat#2125's second starter piece: MSSQLAdminDB used to flip the WHOLE
// DATABASE's predicate form from 'plain' to 'admin' (applyMSSQLCrossTenantOptIn)
// and never flip it back, so the first test in a process to call it left
// every later test -- in every later package sharing the same
// CLEAT_TEST_MSSQL -- running against a predicate no default deployment
// installs. This proves the refcounted restore in mssql_admin.go actually
// puts the form back to 'plain' once the last caller's Cleanup has run.
func TestMSSQLAdminDBRestoresThePlainPredicateAfterUse(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)

	readForm := func(t *testing.T) string {
		t.Helper()
		var form string
		if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
			t.Fatalf("read admin.rls_predicate_form: %v", err)
		}
		return form
	}

	if got := readForm(t); got != "plain" {
		t.Fatalf("predicate form before any MSSQLAdminDB call = %q, want \"plain\" -- "+
			"SetupMSSQLFullSchema should not have changed it", got)
	}

	t.Run("a subtest that calls MSSQLAdminDB", func(t *testing.T) {
		_ = MSSQLAdminDB(t, db)
		if got := readForm(t); got != "admin" {
			t.Fatalf("predicate form while MSSQLAdminDB is held = %q, want \"admin\"", got)
		}
	})

	// The subtest above has returned, so its t.Cleanup(mssqlReleaseAdminDB) has
	// already run -- Cleanup functions run before the parent Run call returns,
	// same as a deferred function returning before its caller does.
	if got := readForm(t); got != "plain" {
		t.Fatalf("predicate form after the only MSSQLAdminDB caller's Cleanup = %q, "+
			"want \"plain\" -- cleat#2125: this used to stay \"admin\" forever, leaking "+
			"into every later test against this database", got)
	}
}

// The refcount, not just the restore: two overlapping callers must not have
// the first one's Cleanup pull the predicate out from under the second.
func TestMSSQLAdminDBRefcountsOverlappingCallers(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)

	readForm := func(t *testing.T) string {
		t.Helper()
		var form string
		if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
			t.Fatalf("read admin.rls_predicate_form: %v", err)
		}
		return form
	}

	// A synthetic *testing.T-like scope: call MSSQLAdminDB twice against the
	// same db (same baseDSN) before either's Cleanup has run, by nesting one
	// subtest inside another rather than sequentially, so the outer caller is
	// still "live" when the inner one's Cleanup fires.
	t.Run("outer", func(t *testing.T) {
		_ = MSSQLAdminDB(t, db)

		t.Run("inner", func(t *testing.T) {
			_ = MSSQLAdminDB(t, db)
		})

		// The inner subtest's Cleanup has already run (refcount 2 -> 1), but
		// the outer caller is still live -- the form must still be "admin".
		if got := readForm(t); got != "admin" {
			t.Fatalf("predicate form after the inner (non-last) caller's Cleanup = %q, "+
				"want \"admin\" -- the outer caller is still live and needs it", got)
		}
	})

	// Now both Cleanups have run (refcount 1 -> 0), so the form must be back
	// to "plain".
	if got := readForm(t); got != "plain" {
		t.Fatalf("predicate form after both callers' Cleanup = %q, want \"plain\"", got)
	}
}
