package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A dropped tenant's workflow definitions go with it, and plugin_defs does not.
//
// cleat#1201: drop-tenant deleted thirteen tables of a tenant's data and left
// the tenant's uploaded WASM in workflow_defs forever, keyed to a tenant_id
// that no longer existed. The command's own usage text asserted the premise --
// "Does not touch workflow_defs/plugin_defs (shared registry, not tenant-owned
// data)" -- and it was half wrong:
//
//	workflow_defs  PRIMARY KEY (tenant_id, name, version)   tenant-owned
//	plugin_defs    PRIMARY KEY (name, version)              no tenant_id at all
//
// Both halves are asserted here. Deleting the definitions is the point; NOT
// deleting plugin_defs is what stops the fix from overshooting into a table
// that really is shared, and it would be invisible without a case.
//
// # Why this is not testing the foreign key
//
// The deletion happens by ON DELETE CASCADE from admin.tenants
// (059_a_dropped_tenants_definitions_go_with_it.sql), not by a DELETE in
// admin.drop_tenant. That is an implementation detail this test deliberately
// does not name: it drives the actual command and asks what survived, so a
// later change from cascade to explicit DELETE, or back, leaves it passing.
// What it pins is the promise -- "permanently delete a tenant and every row of
// its data" -- not the mechanism.
func TestRunDropTenant_TakesTheTenantsDefinitionsAndLeavesPluginDefs(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000001201"
	const defName = "cleatctl-drop-tenant-defs-1201"
	const pluginName = "cleatctl-drop-tenant-shared-plugin-1201"

	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenant, "defs-tenant"); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}
	if err := engine.NewPostgresStore(db).WithTenant(tenant).DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	// plugin_defs has no tenant_id column, which is the whole reason it is
	// exempt. Seeded so that "it survived" is a measurement rather than the
	// vacuous truth of an empty table.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO plugin_defs (name, version, wasm_bytes) VALUES ($1, '1', $2)
		 ON CONFLICT (name, version) DO NOTHING`, pluginName, []byte{0x00, 0x61, 0x73, 0x6d}); err != nil {
		t.Fatalf("seed plugin_defs: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM plugin_defs WHERE name = $1`, pluginName)
	})

	// The precondition, asserted rather than assumed: if the deploy above
	// silently wrote nothing, every assertion below would pass on an empty
	// table and report that a bug was fixed.
	var before int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_defs WHERE tenant_id = $1`, tenant).Scan(&before); err != nil {
		t.Fatalf("counting definitions before the drop: %v", err)
	}
	if before != 1 {
		t.Fatalf("expected 1 definition for the tenant before dropping it, got %d -- "+
			"the test did not set up the state it goes on to assert about", before)
	}

	stdout, stderr := captureOutputs(t, func() {
		runDropTenant(ctx, db, []string{tenant, "--yes"})
	})
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "Deleted tenant "+tenant) {
		t.Fatalf("expected deletion confirmation in stdout, got: %s", stdout)
	}

	// The count table is the operator's only record of what was destroyed --
	// the command's own docs call it the audit trail, since it prints before
	// doing anything. A row removed silently is removed without consent.
	if !strings.Contains(stdout, "workflow_defs") {
		t.Errorf("the pre-deletion count table does not mention workflow_defs:\n%s\n\n"+
			"The definitions are deleted either way, so this is the difference between "+
			"an operator who knows their tenant's uploaded WASM is going and one who does not.", stdout)
	}

	var defsAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_defs WHERE tenant_id = $1`, tenant).Scan(&defsAfter); err != nil {
		t.Fatalf("counting definitions after the drop: %v", err)
	}
	if defsAfter != 0 {
		t.Errorf("%d workflow definition(s) survived the drop, want 0.\n\n"+
			"This is cleat#1201: the tenant is gone, its primary key contains the tenant_id, "+
			"and nothing will ever collect these rows.", defsAfter)
	}

	var pluginsAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_defs WHERE name = $1`, pluginName).Scan(&pluginsAfter); err != nil {
		t.Fatalf("counting plugin_defs after the drop: %v", err)
	}
	if pluginsAfter != 1 {
		t.Errorf("plugin_defs row count for %q is %d after dropping an unrelated tenant, want 1.\n\n"+
			"plugin_defs has no tenant_id column (primary key (name, version)) and is shared "+
			"across tenants. Deleting from it because a tenant went away would take a plugin "+
			"other tenants are still using.", pluginName, pluginsAfter)
	}
}
