package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestThePluginGrantsActuallyGrantAndRevoke is the first test of
// admin.grant_plugin_to_tenant and admin.revoke_plugin_from_tenant. Nothing
// called them before cleat#1480 -- `git grep` found the names only in a
// declared-list test and in a comment.
//
// IT DOES NOT GUARD MIGRATION 070's PIN, AND I MEASURED THAT RATHER THAN
// ASSUMING IT. Setting the pin to a schema that does not exist
//
//	ALTER FUNCTION admin.grant_plugin_to_tenant(TEXT, UUID)
//	    SET search_path = nonexistent_schema;
//
// leaves this test passing. That is not a gap in the test: it is the evidence
// that the pin is INERT. Every name in both functions is already qualified --
// admin.tenant_roles, admin.plugin_tables, and the statements are built with
// format('%I.%I', ...) -- and replace() and format() resolve from pg_catalog,
// which is searched first unless a caller names it explicitly and late. There
// is nothing left for search_path to affect, which is exactly why pinning it
// costs nothing and why an unqualified name introduced later will fail loudly.
//
// So the value here is the round trip, not the pin: these two functions had no
// behavioural test at all, and a pin applied to untested code is a change
// nobody can say was safe.
//
// BOTH FUNCTIONS WARN AND RETURN when the tenant has no role:
//
//	RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping'
//
// and that path SUCCEEDS. A test calling them against a tenant without a role
// would pass whatever they did, and would pass against a body that had been
// deleted. Hence a real role, and hence the privilege asserted ABSENT before
// the grant -- otherwise "the role can select afterwards" is satisfied by a
// role that always could.
func TestThePluginGrantsActuallyGrantAndRevoke(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	tenant := fmt.Sprintf("0148%04d-0000-4000-8000-%012d", time.Now().UnixNano()%10000, time.Now().UnixNano()%1000000000000)
	schema := "tenant_" + sqlSafe(tenant)
	role := "cleat_tenant_" + sqlSafe(tenant)
	const plugin = "probe1480"
	const table = "probe1480_tbl"
	qualified := schema + "." + table

	t.Cleanup(func() {
		db.ExecContext(ctx, `DELETE FROM admin.plugin_tables WHERE plugin_name = $1`, plugin)
		db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		db.ExecContext(ctx, `DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN EXECUTE 'DROP OWNED BY `+role+`'; EXECUTE 'DROP ROLE `+role+`'; END IF; END $$`)
		db.ExecContext(ctx, `DELETE FROM admin.tenant_roles WHERE tenant_id = $1::uuid`, tenant)
		db.ExecContext(ctx, `DELETE FROM admin.tenants WHERE tenant_id = $1::uuid`, tenant)
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1::uuid, $2, $2)`,
		tenant, fmt.Sprintf("probe-1480-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var created *string
	if err := db.QueryRowContext(ctx, `SELECT admin.create_tenant_role($1::uuid, $2)`, tenant, "probe-password-1480-at-least-32-characters-long").Scan(&created); err != nil {
		t.Fatalf("create_tenant_role: %v", err)
	}
	if created == nil {
		// FATAL, NOT SKIP, and check-skips.sh case (c) is right about why: this
		// precondition is always satisfiable here. The test database connects
		// as a superuser, so admin.create_tenant_role's warn-and-return branch
		// -- "cannot create role ... skipping (single-tenant mode)" -- is
		// unreachable. A skip would turn the one environment where this CAN be
		// tested into one where it silently is not.
		t.Fatal("admin.create_tenant_role returned NULL, meaning it could not create the " +
			"role and warned instead. Everything below depends on that role existing, so " +
			"this is a broken fixture rather than an environment without the capability.")
	}

	// The plugin's table has to exist: the GRANT names it.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+qualified+` (tenant_id UUID)`); err != nil {
		t.Fatalf("create plugin table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.plugin_tables (plugin_name, table_name, schema_name, tenant_scoped)
		 VALUES ($1, $2, $3, true) ON CONFLICT DO NOTHING`, plugin, table, schema); err != nil {
		t.Fatalf("register plugin table: %v", err)
	}

	can := func(t *testing.T, when string) bool {
		t.Helper()
		var ok bool
		if err := db.QueryRowContext(ctx,
			`SELECT has_table_privilege($1, $2, 'SELECT')`, role, qualified).Scan(&ok); err != nil {
			t.Fatalf("has_table_privilege (%s): %v", when, err)
		}
		return ok
	}

	// CONTROL. Without this, "the role can select after the grant" is satisfied
	// by a role that could select all along, and the grant need not have run.
	if can(t, "before") {
		t.Fatalf("%s could already SELECT on %s before any grant -- the assertion below "+
			"would pass without admin.grant_plugin_to_tenant doing anything", role, qualified)
	}

	if _, err := db.ExecContext(ctx, `SELECT admin.grant_plugin_to_tenant($1, $2::uuid)`, plugin, tenant); err != nil {
		t.Fatalf("grant_plugin_to_tenant: %v\n\nMigration 070 pins this function's "+
			"search_path to pg_catalog. An error here means the pin removed a schema it "+
			"needed -- which is the failure the pin could introduce and nothing else "+
			"tested for.", err)
	}
	if !can(t, "after grant") {
		t.Errorf("admin.grant_plugin_to_tenant reported success and %s still cannot SELECT "+
			"on %s.\n\nBoth functions warn-and-return when a tenant has no role, and that "+
			"path succeeds -- so a silent no-op looks exactly like this.", role, qualified)
	}

	if _, err := db.ExecContext(ctx, `SELECT admin.revoke_plugin_from_tenant($1, $2::uuid)`, plugin, tenant); err != nil {
		t.Fatalf("revoke_plugin_from_tenant: %v", err)
	}
	if can(t, "after revoke") {
		t.Errorf("admin.revoke_plugin_from_tenant reported success and %s can still SELECT on %s",
			role, qualified)
	}
}

// sqlSafe renders a UUID the way the admin functions do when they build a
// schema or role name, so the fixture and the function agree on the identifier.
func sqlSafe(uuid string) string {
	out := []rune(uuid)
	for i, r := range out {
		if r == '-' {
			out[i] = '_'
		}
	}
	return string(out)
}
