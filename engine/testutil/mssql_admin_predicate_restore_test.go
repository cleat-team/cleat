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

// cleat#2831: a `go test -timeout` panic skips every pending t.Cleanup, so an
// interrupted run can leave the predicate on 'admin' with no live caller in
// ANY process left to restore it. This simulates exactly that shape --
// dirtying the predicate directly, the way an interrupted process's last
// write would leave it, without going through MSSQLAdminDB's own
// Cleanup-skipping path, which a live test cannot trigger without an actual
// timeout -- and checks that the next schema setup (what every MSSQL test
// does first) notices and heals it before anything else runs.
func TestSetupMSSQLFullSchemaSelfHealsAnAdminPredicateLeftDirtyByAnInterruptedRun(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)

	if _, err := db.Exec(`UPDATE admin.rls_predicate_form SET form = N'admin'`); err != nil {
		t.Fatalf("dirty the predicate to simulate an interrupted prior run: %v", err)
	}

	// The self-heal under test: the next schema setup -- what every MSSQL
	// test calls before doing anything else -- must notice the dirty
	// predicate and restore it, rather than trusting it.
	SetupMSSQLFullSchema(t, db)

	var form string
	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("read admin.rls_predicate_form: %v", err)
	}
	if form != "plain" {
		t.Fatalf("predicate form after a fresh SetupMSSQLFullSchema call = %q, want \"plain\" -- "+
			"a dirty predicate left by an interrupted prior run should self-heal here (cleat#2831)", form)
	}
}

// The self-heal above must not clobber a legitimately-live 'admin' state: a
// concurrent or overlapping test elsewhere in this process that is still
// inside an MSSQLAdminDB call needs the predicate to stay 'admin' until its
// own Cleanup runs. Mirrors TestMSSQLAdminDBRefcountsOverlappingCallers'
// concern, but on the setup side rather than the Cleanup side.
func TestSetupMSSQLFullSchemaDoesNotClobberALiveAdminCaller(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)

	_ = MSSQLAdminDB(t, db) // live caller: refcount 1, form now 'admin', Cleanup registered

	// While that caller is still live (its Cleanup has not run -- we are
	// still inside this test), a concurrent test's own schema setup must not
	// rip the predicate out from under it.
	SetupMSSQLFullSchema(t, db)

	var form string
	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("read admin.rls_predicate_form: %v", err)
	}
	if form != "admin" {
		t.Fatalf("predicate form clobbered by a concurrent SetupMSSQLFullSchema call while a live "+
			"MSSQLAdminDB caller still holds it = %q, want \"admin\" (cleat#2831's self-heal must "+
			"guard on the refcount, not just the predicate value)", form)
	}
}
