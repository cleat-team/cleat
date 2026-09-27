package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestDropTenantDeletesFromTheSchemaItIsGiven is cleat#1363.
//
// admin.drop_tenant deleted from seven core tables by UNQUALIFIED name and
// carried no search_path of its own, so every one of those DELETEs resolved
// through the CALLER's search_path. What was measured before the fix: the
// function deleted a decoy table in another schema, left the real rows in
// place, and REPORTED SUCCESS. An operator saw a tenant dropped; the data was
// still there.
//
// THIS IS THAT PROBE WITH ITS OUTCOME INVERTED. The decoy must survive and the
// real row must be gone -- and it matters that BOTH are asserted, because they
// fail in opposite directions:
//
//	decoy survives, real row gone   -> the fix works
//	decoy gone, real row survives   -> the bug, exactly as measured
//	decoy survives, real row survives -> the function deleted NOTHING
//
// The third is why "the decoy survived" cannot stand alone: a function that
// did nothing at all would satisfy it. The pair can only both hold if the
// DELETEs ran AND ran in the right schema.
//
// WHAT A REGRESSION ACTUALLY LOOKS LIKE HERE, because it is not the decoy
// assertion and a reader would misdiagnose it. Falsified by reverting the fix
// in two stages:
//
//	unqualified DELETEs, search_path still pinned to pg_catalog
//	  -> pq: relation "event_history" does not exist (42P01)
//	     Loud, immediately, which is the entire argument for the SET clause:
//	     with every name qualified it affects nothing, so a name that escapes
//	     the rewrite fails instead of silently binding to the caller's schema.
//
//	unqualified DELETEs AND no SET clause  (the pre-fix function)
//	  -> pq: update or delete on table "workflow_defs" violates foreign key
//	     constraint "workflow_instances_def_fkey" (23503)
//	     The misdirect deletes the DECOY's empty workflow_instances, so the real
//	     rows survive; workflow_defs then cannot be deleted while they reference
//	     it. An FK violation here means the DELETEs went to the wrong schema --
//	     not that the fixture is broken.
//
// The decoy carries the SAME tenant id as the real row, deliberately. A decoy
// with a different tenant would be excluded by the WHERE clause and would
// survive against a correct function, a broken one, and one that did nothing --
// the row the policy is supposed to exclude is the only row that measures
// anything.
func TestDropTenantDeletesFromTheSchemaItIsGiven(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	apply032DropTenantMigration(t, adminDB)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenant = "01d00000-0000-4000-8000-000000001363"
	const defName = "drop-tenant-schema-def"
	const decoySchema = "cleat_1363_decoy"

	defer cleanupDropTenantExtras(t, ctx, adminDB, tenant)
	deployDefForTenants(t, adminDB, defName, 1, tenant)
	dropTenantFixture(t, ctx, adminDB, tenant, defName, "schema", false, false)

	// A decoy `workflow_instances` in another schema, holding a row for the
	// same tenant. Shaped only as far as the DELETE needs: a tenant_id column.
	if _, err := adminDB.ExecContext(ctx, `DROP SCHEMA IF EXISTS cleat_1363_decoy CASCADE`); err != nil {
		t.Fatalf("dropping any leftover decoy schema: %v", err)
	}
	defer adminDB.ExecContext(ctx, `DROP SCHEMA IF EXISTS cleat_1363_decoy CASCADE`)
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA cleat_1363_decoy`); err != nil {
		t.Fatalf("creating the decoy schema: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`CREATE TABLE cleat_1363_decoy.workflow_instances (tenant_id UUID)`); err != nil {
		t.Fatalf("creating the decoy table: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO cleat_1363_decoy.workflow_instances (tenant_id) VALUES ($1)`, tenant); err != nil {
		t.Fatalf("seeding the decoy row: %v", err)
	}

	before := countDropTenantRows(t, ctx, adminDB, tenant)
	if before.Instances == 0 {
		t.Fatal("the fixture seeded no workflow_instances rows, so the real-row half of " +
			"this test would pass against a function that deleted nothing")
	}

	// THE ATTACK: the decoy schema first on the caller's search_path. Before the
	// fix this is all it took -- the function's unqualified DELETEs bound to
	// cleat_1363_decoy.workflow_instances.
	//
	// IN A TRANSACTION, WITH set_config(..., true) -- LOCAL. The third argument
	// is the whole reason this is a transaction at all. A session-level
	// set_config leaks: database/sql hands out POOLED connections, so the
	// setting stays on whichever physical connection ran it, a later "restore"
	// may run on a different one, and every subsequent test that draws the
	// polluted connection inherits a search_path naming a schema this test has
	// since dropped. Transaction-local reverts at COMMIT and cannot escape.
	//
	// This is the same idiom the migration itself uses for cleat.tenant_id, and
	// PostgreSQL DDL is transactional, so the DROP SCHEMA and DROP ROLE inside
	// admin.drop_tenant commit with it.
	tx, err := adminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('search_path', 'cleat_1363_decoy, public', true)`); err != nil {
		tx.Rollback()
		t.Fatalf("setting the hostile search_path: %v", err)
	}
	// Confirm the hostile setting is actually in force. Without this the test
	// passes trivially if set_config did not take: the fix would be "verified"
	// against a caller whose search_path was never hostile.
	var inForce string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('search_path')`).Scan(&inForce); err != nil {
		tx.Rollback()
		t.Fatalf("reading back search_path: %v", err)
	}
	if !strings.Contains(inForce, decoySchema) {
		tx.Rollback()
		t.Fatalf("search_path is %q and does not name the decoy schema, so the attack "+
			"this test performs is not being performed", inForce)
	}

	// Explicitly 'public': the schema is now the caller's to state, which is the
	// fix. The hostile search_path above must not change where this deletes.
	if _, err := tx.ExecContext(ctx,
		`SELECT admin.drop_tenant($1, 'public')`, tenant); err != nil {
		tx.Rollback()
		t.Fatalf("admin.drop_tenant: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var decoyRows int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM cleat_1363_decoy.workflow_instances WHERE tenant_id = $1`,
		tenant).Scan(&decoyRows); err != nil {
		t.Fatalf("counting decoy rows: %v", err)
	}
	if decoyRows != 1 {
		t.Errorf("the decoy row in %s.workflow_instances is gone (count=%d, want 1).\n\n"+
			"admin.drop_tenant resolved its DELETE through the CALLER's search_path "+
			"rather than the schema it was given. That is cleat#1363: a caller that "+
			"controls search_path redirects a SECURITY DEFINER delete at a table of "+
			"its choosing, and the function reports success either way.",
			decoySchema, decoyRows)
	}

	after := countDropTenantRows(t, ctx, adminDB, tenant)
	if after.Instances != 0 {
		t.Errorf("public.workflow_instances still holds %d row(s) for the tenant, want 0.\n\n"+
			"Paired with the decoy assertion above this is the more useful half: if the "+
			"decoy ALSO survived, the function deleted nothing at all and 'the decoy "+
			"survived' proved nothing. The two only hold together when the DELETEs ran "+
			"and ran in the schema they were given.", after.Instances)
	}
	if after.Tenants != 0 {
		t.Errorf("admin.tenants still holds %d row(s), want 0", after.Tenants)
	}
}
