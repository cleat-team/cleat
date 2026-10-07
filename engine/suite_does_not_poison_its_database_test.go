//go:build cgo

package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The engine suite used to poison its own database.
//
// engine/drop_tenant_test.go's resetToOriginal001DropTenant re-applied the
// whole of migrations/postgres/001_schema.sql to recover one pre-032 function.
// 001 defines FIVE functions with CREATE OR REPLACE, and two of them have later
// migrations that fix them, so the helper silently reverted a migration it was
// never aimed at: 034's cleat.assert_tenant_set, back to the body that treats
// an empty tenant id as set.
//
// schema_migrations still recorded version 34, and the runner only applies
// versions it has not recorded, so nothing repaired it. Measured 2026-09-02: a
// full `go test ./engine/` run finished GREEN and left the database in a state
// where the NEXT run failed TestAssertTenantSetRejectsEmptyStringLikeNull --
// a tenant/RLS failure with no visible connection to the test that caused it,
// appearing and disappearing according to what had run against that database
// before.
//
// That is the shape CLAUDE.md's "is this result real?" section is about, and it
// cost most of a session to attribute: it looks exactly like a flaky test, and
// it looks exactly like an unrelated change having broken RLS.

// TestTheSuiteLeavesTheMigratedSchemaIntact checks the invariant directly
// rather than through its symptom.
//
// Asserting "TestAssertTenantSetRejectsEmptyStringLikeNull still passes" would
// only catch the one function that happened to be damaged. The property that
// actually matters is that the shipped, migrated definition is what is
// installed -- so this compares the live function body against the migration
// that last defines it, which catches any future helper that reverts any
// function for any reason.
func TestTheSuiteLeavesTheMigratedSchemaIntact(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	// 034 is the highest-numbered migration defining cleat.assert_tenant_set,
	// so its body -- not 001's -- is what a correctly migrated database has.
	// Re-derive the "highest-numbered" part with
	//   grep -ln 'FUNCTION.*assert_tenant_set' migrations/postgres/*.sql
	// which is the §1.1 discipline for anything defined by CREATE OR REPLACE.
	var body string
	err := db.QueryRowContext(ctx, `
		SELECT p.prosrc
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.proname = 'assert_tenant_set' AND n.nspname = 'cleat'`).Scan(&body)
	if err != nil {
		t.Fatalf("reading cleat.assert_tenant_set from the test database: %v", err)
	}

	// The empty-string guard is 034's whole contribution. Match on the guard
	// itself, NOT on the exception message: "cleat.tenant_id is not set"
	// appears in BOTH the 001 and 034 bodies, so a check for it passes against
	// the broken function. I made exactly that mistake while diagnosing this
	// and it reported the reverted database as healthy.
	if !strings.Contains(body, "tid = ''") {
		t.Fatalf("cleat.assert_tenant_set is missing 034's empty-string guard.\n\n"+
			"live body:\n%s\n\n"+
			"Some test in this package reverted it -- most likely by re-applying a "+
			"whole migration file to recover one object from it. schema_migrations "+
			"still records the version as applied, so no migration run will repair "+
			"this: the database stays broken for every subsequent run against it, "+
			"and the failure surfaces in an unrelated tenant/RLS test.\n\n"+
			"To recover this database: DELETE FROM schema_migrations WHERE version='34' "+
			"and re-run any test that calls SetupFullSchema.", body)
	}

	// cleat#2449, the sibling defect to the one this file is named for: a
	// fixture that INSTALLS a superseded function signature and never drops
	// it, rather than one that reverts a current one. drop_tenant_test.go's
	// resetToOriginal001DropTenant does exactly this on purpose (three tests
	// need the pre-032, one-argument admin.drop_tenant to pin a data-loss
	// bug) -- the defect was never calling it, it was never cleaning up
	// after. Checked here, at the granularity the property actually has
	// (no admin function keeps more than one signature), rather than by
	// naming drop_tenant specifically: that also catches the next helper
	// that does the same thing to a different function.
	rows, err := db.QueryContext(ctx, `
		SELECT p.proname, COUNT(*)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'admin'
		GROUP BY p.proname
		HAVING COUNT(*) > 1
	`)
	if err != nil {
		t.Fatalf("checking for superseded admin function overloads: %v", err)
	}
	defer rows.Close()
	var overloaded []string
	for rows.Next() {
		var name string
		var n int
		if scanErr := rows.Scan(&name, &n); scanErr != nil {
			t.Fatalf("scanning admin function overload row: %v", scanErr)
		}
		overloaded = append(overloaded, fmt.Sprintf("%s (%d overloads)", name, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating admin function overload rows: %v", err)
	}
	if len(overloaded) > 0 {
		t.Fatalf("admin schema has superseded function overload(s) that a test fixture "+
			"installed and never dropped: %s -- a database in this state is exactly "+
			"cleat#2449 (a stale, unpinned SECURITY DEFINER overload cleatctl check-db "+
			"would flag on a real deployment). Return a cleanup closure that drops the "+
			"old signature by its exact arguments, and defer it AFTER this fixture's "+
			"own defer that closes the database connection (LIFO order then runs the "+
			"drop first) -- NOT t.Cleanup, which runs after every plain defer in the "+
			"test has already fired, so it would try to drop the function on an "+
			"already-closed connection. See resetToOriginal001DropTenant's own doc "+
			"comment (drop_tenant_test.go) for the same fix applied once already.",
			strings.Join(overloaded, ", "))
	}
}
