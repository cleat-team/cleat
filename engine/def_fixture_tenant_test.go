package engine

// Shared fixture helper for the D7 key change (IMPROVEMENT-PLAN 3.77).
//
// Before D7, workflow_defs was keyed by (name, version) with no tenant, so one
// deployed definition satisfied every tenant's foreign key and a fixture could
// deploy once and then start runs as several tenants. Under
// (tenant_id, name, version) each tenant owns its own row, and
// workflow_instances_def_fkey now carries tenant_id -- so a run started by a
// tenant that has not deployed the definition is refused.
//
// That refusal is the point of the change, so the fixtures move rather than the
// constraint. This helper is what they move to: deploy the definition once per
// tenant that will reference it.

import (
	"context"
	"database/sql"
	"testing"
)

// deployDefForTenants deploys one definition, once for each named tenant.
//
// Takes an admin (RLS-bypassing) handle because that is what these fixtures
// already hold, but goes through DeployWorkflowDef rather than a raw INSERT:
// a fixture that writes the row itself would keep passing if the deploy path
// stopped recording the tenant, which is half of what 3.12 fixed.
func deployDefForTenants(t *testing.T, adminDB *sql.DB, defName string, version int, tenants ...string) {
	t.Helper()
	for _, tn := range tenants {
		if err := NewPostgresStore(adminDB).WithTenant(tn).DeployWorkflowDef(
			context.Background(), &WorkflowDef{
				Name: defName, Version: version,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
			t.Fatalf("deploy %q v%d for tenant %s: %v", defName, version, tn, err)
		}
	}
}
