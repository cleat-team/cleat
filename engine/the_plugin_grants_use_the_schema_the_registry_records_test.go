package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2389. admin.grant_plugin_to_tenant and admin.revoke_plugin_from_tenant
// computed the schema they act on as 'tenant_<uuid>' and ignored
// admin.plugin_tables.schema_name -- the column migration 066 added for exactly
// this, and the one admin.drop_tenant already reads.
//
// THE SIBLING TEST COULD NOT SEE IT, WHICH IS WHY THIS FILE EXISTS.
// TestThePluginGrantsActuallyGrantAndRevoke creates its plugin table IN the
// tenant schema --
//
//	qualified := schema + "." + table
//
// -- and registers it with that same schema. So its fixture is precisely the
// case where the computed name and the recorded name agree: the function's
// assumption is baked into the setup meant to test it, and that test passed
// throughout. A fixture that places the table where the code assumes it is
// cannot ask whether the code assumed correctly.
//
// The case that fails is the ORDINARY one. plugin/migration.go pins
// search_path = public, so a plugin table lands in public whatever the worker's
// --schema says, and the registry records 'public'. Measured against a real
// plugin with such a row, before this fix:
//
//	ERROR:  relation "tenant_617aa860_cf41_4f27_85e9_aaa8aa478c59.tenant_quota"
//	        does not exist
//	CONTEXT:  ... GRANT ... ON tenant_617aa860_....tenant_quota TO cleat_tenant_...
//
// It was latent rather than absent: nothing populated the registry until
// cleat#2343 (cleat#2367), so the loop was zero-iteration and these functions
// returned success without attempting anything.
func TestThePluginGrantsUseTheSchemaTheRegistryRecords(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	ctx := context.Background()

	tenant := fmt.Sprintf("0238%04d-0000-4000-8000-%012d", time.Now().UnixNano()%10000, time.Now().UnixNano()%1000000000000)
	safe := sqlSafe(tenant)
	schema := "tenant_" + safe
	role := "cleat_tenant_" + safe
	plugin := "probe2389"
	// Unique per run: this table lives in public, shared with every other test
	// in this database.
	table := "probe2389_tbl_" + safe[len(safe)-8:]

	// THE POINT OF THE FIXTURE: public, not the tenant schema. See the header.
	qualified := "public." + table

	t.Cleanup(func() {
		db.ExecContext(ctx, `DROP TABLE IF EXISTS `+qualified)
		db.ExecContext(ctx, `DELETE FROM admin.plugin_tables WHERE plugin_name LIKE $1`, plugin+"%")
		db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		db.ExecContext(ctx, `DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN EXECUTE 'DROP OWNED BY `+role+`'; EXECUTE 'DROP ROLE `+role+`'; END IF; END $$`)
		db.ExecContext(ctx, `DELETE FROM admin.tenant_roles WHERE tenant_id = $1::uuid`, tenant)
		db.ExecContext(ctx, `DELETE FROM admin.tenants WHERE tenant_id = $1::uuid`, tenant)
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1::uuid, $2, $2)`,
		tenant, fmt.Sprintf("probe-2389-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var created *string
	if err := db.QueryRowContext(ctx, `SELECT admin.create_tenant_role($1::uuid, $2)`,
		tenant, "probe-password-2389-at-least-32-characters-long").Scan(&created); err != nil {
		t.Fatalf("create_tenant_role: %v", err)
	}
	if created == nil {
		// FATAL, not skip, for the reason the sibling test gives: the test
		// database connects as a superuser, so the warn-and-return branch is
		// unreachable here and everything below needs the role to exist.
		t.Fatal("admin.create_tenant_role returned NULL -- the fixture is broken, not the environment")
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+qualified+` (tenant_id UUID)`); err != nil {
		t.Fatalf("create plugin table: %v", err)
	}
	// Registered against the schema the table is ACTUALLY in. That is what the
	// column is for, and what the function used to ignore.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.plugin_tables (plugin_name, table_name, schema_name, tenant_scoped)
		 VALUES ($1, $2, 'public', true) ON CONFLICT DO NOTHING`, plugin, table); err != nil {
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

	// CONTROL. "The role can select afterwards" is otherwise satisfied by a
	// role that could select all along, and the grant need not have run -- the
	// same control, for the same reason, as the sibling test's.
	if can(t, "before") {
		t.Fatalf("%s could already SELECT %s before any grant, so the assertion below would "+
			"pass without admin.grant_plugin_to_tenant doing anything", role, qualified)
	}

	// THE CASE THAT USED TO RAISE. The registry records public; the function
	// used to GRANT on tenant_<uuid> and fail with "relation does not exist".
	if _, err := db.ExecContext(ctx, `SELECT admin.grant_plugin_to_tenant($1, $2::uuid)`, plugin, tenant); err != nil {
		t.Fatalf("admin.grant_plugin_to_tenant raised for a plugin whose registered schema is "+
			"public: %v\n\nThis is cleat#2389. The function must act on "+
			"admin.plugin_tables.schema_name, not on a schema derived from the tenant id -- "+
			"public is where plugin migrations actually put the table.", err)
	}
	if !can(t, "after grant") {
		t.Errorf("admin.grant_plugin_to_tenant returned success and %s still cannot SELECT %s.\n"+
			"Both functions warn-and-return when a tenant has no role, and that path SUCCEEDS, "+
			"so a silent no-op looks exactly like this.", role, qualified)
	}

	if _, err := db.ExecContext(ctx, `SELECT admin.revoke_plugin_from_tenant($1, $2::uuid)`, plugin, tenant); err != nil {
		t.Fatalf("admin.revoke_plugin_from_tenant raised, and it carries the identical bug: %v", err)
	}
	if can(t, "after revoke") {
		t.Errorf("admin.revoke_plugin_from_tenant returned success and %s can still SELECT %s",
			role, qualified)
	}

	// CONTROL for the error itself, and it is a different control from the one
	// above. A plugin with no registry rows makes the loop zero-iteration, and
	// that must still return cleanly. Without this, a "fix" that made the
	// function raise for every plugin would pass everything above -- and the
	// empty loop is the state these functions were in for their whole life
	// before cleat#2343, so it is the one behaviour worth pinning.
	if _, err := db.ExecContext(ctx, `SELECT admin.grant_plugin_to_tenant($1, $2::uuid)`,
		"no-such-plugin-2389", tenant); err != nil {
		t.Errorf("admin.grant_plugin_to_tenant raised for a plugin with no registry rows: %v\n"+
			"An empty loop must return cleanly. This failing means the change broke the "+
			"zero-iteration path rather than the schema lookup.", err)
	}
}
