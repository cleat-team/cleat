package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestCheckDBDetectsASupersededUnpinnedFunctionOverload constructs the exact
// shape of cleat#2449 against a REAL PostgreSQL server: a superseded,
// unpinned SECURITY DEFINER overload coexisting with the current signature.
//
// WHY THIS CANNOT BE REPRODUCED BY POINTING check-db AT AN ORDINARY DATABASE.
// migrations/postgres/ ships only the LAST signature of each routine --
// 001_schema.sql is a pg_dump baseline of a database's current state
// (#2416's rebaseline), not a replay of every transition a routine went
// through. A fresh build therefore never has a superseded overload to find,
// and neither does testutil's shared, persistent test database, which is
// always migrated forward from that same baseline. The only way to exercise
// the "found one" path against a real server is to create the stale overload
// directly, the way it can arise in practice: a manual statement run outside
// the migration runner, or an (unsupported) in-place upgrade attempted across
// a rebaseline boundary -- see cleat#2449's own comment for why an ordinary,
// version-tracked migration run cannot produce this shape on its own.
//
// ONE TEST, BOTH DIRECTIONS. Asserting only "flagged when present" would pass
// against a check that flags every admin-schema function, or every function
// named drop_tenant regardless of overload count -- the mock-based tests in
// checkdb_test.go already cover that distinction with a scripted result, but
// this is the one place the REAL SQL is exercised, so the same distinction is
// asserted here too: dropped, the same database must read clean again.
func TestCheckDBDetectsASupersededUnpinnedFunctionOverload(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	testutil.SetupMinimalSchema(t, db, testutil.DialectPostgres)

	// Baseline: the shared test database, on the current schema, must not
	// already read as having a stale overload -- otherwise the "flagged"
	// assertion below would prove nothing about THIS function creating it.
	//
	// _, baseline, NOT baseline, _: the FUNCTION OVERLOADS line goes to
	// STDERR (checkdb.go's fmt.Fprintf), and withExitPanicOutput returns
	// (stdout, stderr) in that order. Checking stdout here made this premise
	// guard dead code -- found by cleat-review, and confirmed real: a
	// database polluted by engine/drop_tenant_test.go's
	// resetToOriginal001DropTenant (fixed in the same PR, cleat#2449) made
	// the CREATE FUNCTION below fail on "already exists" instead of this
	// guard ever firing.
	_, baseline := withExitPanicOutput(t, func() {
		runCheckDB(ctx, db, dialectPostgres, "", nil)
	})
	if strings.Contains(baseline, "FUNCTION OVERLOADS") {
		t.Fatalf("UNMEASURED: the shared test database already reports a stale function "+
			"overload before this test created one -- this test's premise requires a clean "+
			"starting state, which some earlier test in this binary left dirty:\n%s", baseline)
	}

	// The stale overload: admin.drop_tenant(uuid), one argument -- exactly
	// migrations/postgres' pre-#2416 signature (032_drop_tenant_deletes_tenant_data.sql),
	// SECURITY DEFINER with no SET search_path, coexisting with 001_schema.sql's
	// current two-argument admin.drop_tenant(uuid, text). The body only needs
	// to be valid PL/pgSQL; it is never called.
	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION admin.drop_tenant(p_tenant_id uuid) RETURNS void AS $$
		BEGIN
			RAISE EXCEPTION 'superseded overload, cleat#2449 test fixture -- must never run';
		END;
		$$ LANGUAGE plpgsql SECURITY DEFINER
	`); err != nil {
		t.Fatalf("create the stale one-argument overload: %v", err)
	}
	// However the test ends, the shared database must not keep this overload
	// -- it would make every OTHER test in this binary that runs check-db
	// (there are several) fail with a defect this test manufactured on
	// purpose. Dropped by its exact old signature, not by name, since
	// DROP FUNCTION admin.drop_tenant alone is ambiguous with two overloads
	// present.
	defer func() {
		if _, err := db.ExecContext(ctx,
			`DROP FUNCTION IF EXISTS admin.drop_tenant(uuid)`); err != nil {
			t.Errorf("cleanup: drop the stale overload this test created: %v", err)
		}
	}()

	stdout, stderr := withExitPanicOutput(t, func() {
		runCheckDB(ctx, db, dialectPostgres, "", nil)
	})
	if !strings.Contains(stderr, "FUNCTION OVERLOADS") {
		t.Fatalf("check-db did not report the stale overload it should have found:\n"+
			"stdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stderr, "drop_tenant") {
		t.Errorf("the FUNCTION OVERLOADS warning does not name drop_tenant:\n%s", stderr)
	}
	if !strings.Contains(stderr, "DEGRADED") {
		t.Errorf("a superseded, unpinned SECURITY DEFINER overload must mark the database "+
			"DEGRADED, got stderr: %s", stderr)
	}

	// The other direction, in the SAME run: drop the overload and require
	// the same database to read clean again, ruling out a check that fires
	// on admin.drop_tenant (or on the admin schema) unconditionally.
	if _, err := db.ExecContext(ctx,
		`DROP FUNCTION IF EXISTS admin.drop_tenant(uuid)`); err != nil {
		t.Fatalf("drop the stale overload before the clean re-check: %v", err)
	}
	stdout, stderr = withExitPanicOutput(t, func() {
		runCheckDB(ctx, db, dialectPostgres, "", nil)
	})
	if strings.Contains(stderr, "FUNCTION OVERLOADS") {
		t.Errorf("check-db still reports a stale overload after it was dropped:\n"+
			"stdout: %s\nstderr: %s", stdout, stderr)
	}
}
