package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// No function in the admin schema may grant EXECUTE to PUBLIC. cleat#1365,
// cleat#1373.
//
// WHY A PREDICATE OVER THE WHOLE SCHEMA RATHER THAN FOUR ASSERTIONS. Four of
// the six functions in admin held PUBLIC's EXECUTE, and none of them was
// written that way on purpose -- they simply kept PostgreSQL's default, which
// is EXECUTE TO PUBLIC for every new function. 023 and 024 are the two that
// do it right, by assigning an owner and granting explicitly.
//
// So the defect is not four functions. It is that the default is open and
// nothing looks. Function number seven will inherit the same default, and a
// test naming today's six would pass while it did. This asserts the property.
//
// WHAT MAKES IT ABLE TO FAIL. A permission test can pass because the fixture
// never had reach in the first place, so this does not stop at "PUBLIC has no
// EXECUTE": it checks that the scan saw the functions at all, and the
// companion test below checks that a role WITH a grant can still call one.
// "Nobody can execute anything" would satisfy a weaker version of this and is
// a broken database, not a fixed one.
func TestNoAdminFunctionGrantsExecuteToPublic(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `
		SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		       has_function_privilege('public', p.oid, 'EXECUTE'),
		       coalesce(p.proacl::text, '<null>')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'admin'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("query admin function ACLs: %v", err)
	}
	defer rows.Close()

	var public []string
	examined := 0
	for rows.Next() {
		var sig, acl string
		var isPublic bool
		if err := rows.Scan(&sig, &isPublic, &acl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		examined++
		if isPublic {
			public = append(public, "  admin."+sig+"\n    acl="+acl)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// The floor. A schema with no functions in it satisfies "none of them is
	// public" perfectly, and that is the state a broken migration run leaves.
	if examined < 4 {
		t.Fatalf("found only %d functions in schema admin; the scan is not "+
			"reaching them, so its silence means nothing", examined)
	}
	if len(public) > 0 {
		t.Errorf("%d of %d functions in schema admin grant EXECUTE to PUBLIC:\n%s\n\n"+
			"PostgreSQL's default for a new function is EXECUTE TO PUBLIC, so "+
			"this is what a function gets by being written rather than by "+
			"anyone deciding it. These are SECURITY DEFINER and execute as "+
			"their owner, which means RLS does not apply to them: cleat#1365 "+
			"is an ordinary tenant login role destroying another tenant's data "+
			"through exactly this. Add a REVOKE to a migration, next to 065's.",
			len(public), examined, strings.Join(public, "\n"))
	}
}

// And the other half: revoking from PUBLIC must not have revoked from the
// application role, which is the failure mode a REVOKE invites.
//
// This is the known-good call that makes the test above mean something. A
// database where nobody can execute anything passes "no function is public"
// and is broken; the two assertions together say the grant is narrowed rather
// than removed.
func TestTheAppRoleKeepsExecuteOnTheAdminFunctions(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='cleat_app')`).Scan(&exists); err != nil {
		t.Fatalf("look up cleat_app: %v", err)
	}
	if !exists {
		t.Fatal("cleat_app does not exist; 005_app_role.sql creates it and this " +
			"test is about what 065 does to its grants, so a missing role means " +
			"the fixture is wrong rather than that there is nothing to check")
	}

	// Enumerated from the catalogue, not listed here. The first version of
	// this test named four signatures, and migration 064 changed one of them
	// and added a function within the hour -- the same staleness 065's header
	// records. A list of names in a test about "every function in a schema" is
	// the bug the test is for, one layer up.
	rows, err := db.QueryContext(ctx, `
		SELECT 'admin.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		       has_function_privilege('cleat_app', p.oid, 'EXECUTE')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'admin'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("query admin function grants: %v", err)
	}
	defer rows.Close()

	examined := 0
	for rows.Next() {
		var sig string
		var ok bool
		if err := rows.Scan(&sig, &ok); err != nil {
			t.Fatalf("scan: %v", err)
		}
		examined++
		if !ok {
			t.Errorf("cleat_app has no EXECUTE on %s.\n\n"+
				"065 revokes from PUBLIC and re-grants to cleat_app. "+
				"REVOKE ... FROM PUBLIC does not touch a named grantee, so "+
				"005_app_role.sql's explicit grant should survive either way. "+
				"If this fails, something revoked from the role rather than "+
				"from PUBLIC -- which is the failure mode a blanket REVOKE "+
				"invites and the reason this test exists beside the other one.", sig)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if examined < 4 {
		t.Fatalf("found only %d functions in schema admin; this test cannot "+
			"say anything about grants it never looked at", examined)
	}

}
