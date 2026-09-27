package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2402. admin.grant_plugin_to_tenant and admin.revoke_plugin_from_tenant
// walked admin.plugin_tables by plugin NAME alone:
//
//	SELECT t.schema_name, t.table_name FROM admin.plugin_tables t
//	WHERE t.plugin_name = p_plugin_name
//
// with no schema predicate, inside a SECURITY DEFINER function. Migration 066
// gave that table a `schema_name` column for a stated reason -- `admin` is not
// moved by `--schema`, so ONE REGISTRY CAN HOLD ENTRIES FROM MORE THAN ONE
// INSTALL -- and moved the PK to (plugin_name, schema_name, table_name). So a
// call naming one install's plugin granted the tenant's role access to EVERY
// install's tables of that name, and revoked from all of them.
//
// THE FALSIFICATION IS THE SECOND ASSERTION BELOW, and it is the whole reason
// this file exists. "The role can select the right install's table" is
// satisfied by the defective code: that table is in the registry too. Only
// "and CANNOT select the OTHER install's table" distinguishes them, and it goes
// green the moment the predicate is removed. A scoping test whose known-positive
// is the happy path passes on an empty predicate.
//
// WHY TWO ROWS OF THE SAME PLUGIN NAME IS THE ORDINARY CASE, not a constructed
// one: migration 066's own header licenses it, and `admin` living outside
// `--schema` is what puts both installs' rows in one table.
func TestThePluginGrantIsScopedToTheInstall(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	tenant := fmt.Sprintf("0240%04d-0000-4000-8000-%012d", time.Now().UnixNano()%10000, time.Now().UnixNano()%1000000000000)
	safe := sqlSafe(tenant)
	role := "cleat_tenant_" + safe
	const plugin = "probe2402"
	// Unique per run: the "other install" schema outlives nothing, but the
	// plugin row is keyed by name and this database is shared.
	suffix := safe[len(safe)-8:]
	table := "probe2402_tbl_" + suffix
	// The install under test, and a SECOND install registering the same plugin
	// name. Two schemas, one registry.
	schemaUnderTest := "tenant_" + safe
	schemaOther := "probe2402_other_" + suffix
	qualifiedUnderTest := schemaUnderTest + "." + table
	qualifiedOther := schemaOther + "." + table

	t.Cleanup(func() {
		db.ExecContext(ctx, `DELETE FROM admin.plugin_tables WHERE plugin_name = $1`, plugin)
		db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schemaUnderTest+` CASCADE`)
		db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schemaOther+` CASCADE`)
		db.ExecContext(ctx, `DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN EXECUTE 'DROP OWNED BY `+role+`'; EXECUTE 'DROP ROLE `+role+`'; END IF; END $$`)
		db.ExecContext(ctx, `DELETE FROM admin.tenant_roles WHERE tenant_id = $1::uuid`, tenant)
		db.ExecContext(ctx, `DELETE FROM admin.tenants WHERE tenant_id = $1::uuid`, tenant)
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1::uuid, $2, $2)`,
		tenant, fmt.Sprintf("probe-2402-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var created *string
	if err := db.QueryRowContext(ctx, `SELECT admin.create_tenant_role($1::uuid, $2)`,
		tenant, "probe-password-2402-at-least-32-characters-long").Scan(&created); err != nil {
		t.Fatalf("create_tenant_role: %v", err)
	}
	if created == nil {
		// FATAL, not skip, for the reason the sibling tests give: the test
		// database connects as a superuser, so the warn-and-return branch is
		// unreachable and everything below needs the role to exist.
		t.Fatal("admin.create_tenant_role returned NULL -- the fixture is broken, not the environment")
	}

	// Both installs have the table, and both are registered under their own
	// schema -- which is what makes the two rows distinguishable at all.
	for _, schema := range []string{schemaUnderTest, schemaOther} {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+schema+`.`+table+` (tenant_id UUID)`); err != nil {
			t.Fatalf("create plugin table in %s: %v", schema, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.plugin_tables (plugin_name, table_name, schema_name, tenant_scoped)
		 VALUES ($1, $2, $3, true), ($1, $2, $4, true) ON CONFLICT DO NOTHING`,
		plugin, table, schemaUnderTest, schemaOther); err != nil {
		t.Fatalf("register plugin tables: %v", err)
	}

	can := func(t *testing.T, when, qualified string) bool {
		t.Helper()
		var ok bool
		if err := db.QueryRowContext(ctx,
			`SELECT has_table_privilege($1, $2, 'SELECT')`, role, qualified).Scan(&ok); err != nil {
			t.Fatalf("has_table_privilege (%s, %s): %v", when, qualified, err)
		}
		return ok
	}

	// CONTROL. Without this, the assertion after the grant is satisfied by a
	// role that could select all along, and the grant need not have run.
	if can(t, "before", qualifiedUnderTest) {
		t.Fatalf("%s could already SELECT %s before any grant -- the assertion below "+
			"would pass without admin.grant_plugin_to_tenant doing anything",
			role, qualifiedUnderTest)
	}

	if _, err := db.ExecContext(ctx,
		`SELECT admin.grant_plugin_to_tenant($1, $2::uuid, $3)`,
		plugin, tenant, schemaUnderTest); err != nil {
		t.Fatalf("admin.grant_plugin_to_tenant: %v", err)
	}

	if !can(t, "after grant", qualifiedUnderTest) {
		t.Errorf("admin.grant_plugin_to_tenant returned success and %s still cannot SELECT %s.\n"+
			"Both functions warn-and-return when a tenant has no role, and that path SUCCEEDS, "+
			"so a silent no-op looks exactly like this.", role, qualifiedUnderTest)
	}

	// THE ASSERTION THIS FILE EXISTS FOR. Without the scope predicate the loop
	// walks both rows and grants on both schemas, and only this line notices.
	if can(t, "after grant", qualifiedOther) {
		t.Errorf("admin.grant_plugin_to_tenant granted on %s, which belongs to a DIFFERENT "+
			"install.\n\nThe call named schema %q and the registry holds a second row for "+
			"plugin %q under schema %q. This is cleat#2402: the walk must be scoped by "+
			"admin.plugin_tables.schema_name, or one tenant's plugin grant reaches every "+
			"install of that plugin name in the database.",
			qualifiedOther, schemaUnderTest, plugin, schemaOther)
	}

	// The same function on the way back out, for the same reason: a revoke
	// scoped to one install must not take privileges from another's tables.
	if _, err := db.ExecContext(ctx,
		`SELECT admin.revoke_plugin_from_tenant($1, $2::uuid, $3)`,
		plugin, tenant, schemaUnderTest); err != nil {
		t.Fatalf("admin.revoke_plugin_from_tenant: %v", err)
	}
	if can(t, "after revoke", qualifiedUnderTest) {
		t.Errorf("admin.revoke_plugin_from_tenant returned success and %s can still SELECT %s",
			role, qualifiedUnderTest)
	}
}
