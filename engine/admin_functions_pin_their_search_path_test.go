package engine

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestEverySecurityDefinerAdminFunctionPinsItsSearchPath is cleat#1480.
//
// A SECURITY DEFINER function that sets no search_path resolves every
// unqualified name through the CALLER's path, while running with the owner's
// privileges. cleat#1363 measured what that costs on admin.drop_tenant: it
// deleted a decoy table in another schema, left the real rows, and reported
// success.
//
// IT READS pg_proc, NOT THE MIGRATION FILES, AND THAT IS THE POINT.
// A file scan cannot see supersession. admin.drop_tenant's original definition
// in 001_schema.sql sets no search_path either, and migration 069 pinned it
// four files later -- so a grep over migrations/ reports a function as exposed
// that has been fixed, and would equally miss one that a later migration
// un-pinned. Only the catalog knows which definition is live.
//
// IT FAILS ON THE NEXT FUNCTION ADDED, rather than on a list. The exempt set
// below is deliberately about a PROPERTY -- invoker-rights -- and not a roster
// of names, so a new SECURITY DEFINER routine is caught without anyone
// remembering to add it here.
func TestEverySecurityDefinerAdminFunctionPinsItsSearchPath(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	rows, err := db.QueryContext(context.Background(), `
		SELECT n.nspname || '.' || p.proname,
		       p.prosecdef,
		       COALESCE(array_to_string(p.proconfig, ','), '')
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('admin', 'cleat')
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("reading pg_proc: %v", err)
	}
	defer rows.Close()

	var secdef, unpinned, invoker []string
	for rows.Next() {
		var name, cfg string
		var isSecDef bool
		if err := rows.Scan(&name, &isSecDef, &cfg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !isSecDef {
			invoker = append(invoker, name)
			continue
		}
		secdef = append(secdef, name)
		if !strings.Contains(cfg, "search_path=") {
			unpinned = append(unpinned, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(secdef)
	sort.Strings(unpinned)

	// VACUITY. Zero SECURITY DEFINER functions means the schema did not apply,
	// the query is wrong, or this is not a cleat database -- and every
	// assertion below would pass. There are several; the guard asserts the
	// PROPERTY that some exist rather than how many, because the count grows.
	if len(secdef) == 0 {
		t.Fatal("found no SECURITY DEFINER functions in the admin or cleat schemas. " +
			"cleat defines several (admin.drop_tenant, admin.claim_workflows, ...), so " +
			"zero means this guard checked nothing rather than that the tree is clean.")
	}

	if len(unpinned) > 0 {
		t.Errorf("%d SECURITY DEFINER function(s) in admin/cleat set no search_path: %v\n\n"+
			"Each resolves every unqualified name -- including builtins like replace() and "+
			"format(), since pg_catalog is searched first only IMPLICITLY -- through the "+
			"CALLER's path, while running with the owner's privileges. That is cleat#1363's "+
			"shape: admin.drop_tenant deleted a decoy table in another schema, left the real "+
			"rows, and reported success.\n\n"+
			"Pin it with ALTER FUNCTION ... SET search_path = pg_catalog in a new migration, "+
			"rather than by editing the file that defines the function -- see migration 070. "+
			"The pin also makes the function VERIFIABLE: with everything qualified there is "+
			"nothing left for the setting to affect, so a name that escapes a later edit "+
			"fails loudly instead of binding to whoever called.\n\n"+
			"all SECURITY DEFINER functions here (%d): %v",
			len(unpinned), unpinned, len(secdef), secdef)
	}
	t.Logf("SECURITY DEFINER functions checked: %d, all pinned (%d invoker-rights functions "+
		"are out of scope: %v)", len(secdef), len(invoker), invoker)
}
