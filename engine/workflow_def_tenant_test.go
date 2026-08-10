package engine

// Found while fixing IMPROVEMENT-PLAN 3.10, whose two-tenant test could not
// deploy a definition of the same name from both stores.
//
// workflow_defs' primary key is (name, version) on all three dialects, with no
// tenant in it, and definition names are chosen by whoever deploys. All three
// DeployWorkflowDef implementations upsert on that key, so the second tenant to
// deploy a given name does not collide -- it overwrites. That is not an
// information leak, it is code replacement: tenant B decides what tenant A's
// workflows execute, by picking a name.
//
// This file asserts the bounded property (3.12): a deploy must never replace a
// definition owned by another tenant. It deliberately does NOT assert that two
// tenants can each hold their own "order-processor" -- that needs the primary
// key to carry the tenant, and with it three foreign keys per dialect and an
// audit of ~96 query sites. The name is still a global namespace afterwards;
// what changes is that squatting it is loud rather than silent.

import (
	"bytes"
	"context"
	"testing"
)

// readDefTenantID reads workflow_defs.tenant_id straight out of the database,
// on an administrative connection rather than through the store.
//
// Deliberately not via the store: on PostgreSQL the store's connection is the
// RLS-enforcing one, which is the layer under test here, and
// WorkflowDef carries no TenantID field to read anyway. The admin connection
// sees every row, so a wrong owner shows up as a wrong owner rather than as an
// empty result.
func readDefTenantID(t *testing.T, backend StoreBackend, name string, version int) string {
	t.Helper()
	// The handle comes from adminDBFor rather than from a switch of its own.
	// The point of this test is to read a row belonging to *another* tenant --
	// that is what "recorded the deploying tenant" means -- so a handle subject
	// to the tenant fence cannot answer it. SQL Server's was, and the test
	// failed with "sql: no rows in result set" on a definition that had
	// deployed successfully.
	db := adminDBFor(t, backend)

	var q string
	switch backend.Name() {
	case "postgres":
		q = `SELECT tenant_id::text FROM workflow_defs WHERE name = $1 AND version = $2`
	case "mysql":
		q = `SELECT tenant_id FROM workflow_defs WHERE name = ? AND version = ?`
	case "mssql":
		q = `SELECT LOWER(CONVERT(NVARCHAR(36), tenant_id)) FROM workflow_defs WHERE name = @p1 AND version = @p2`
	default:
		t.Fatalf("readDefTenantID: unknown backend %q", backend.Name())
	}

	var owner string
	if err := db.QueryRow(q, name, version).Scan(&owner); err != nil {
		t.Fatalf("read workflow_defs.tenant_id: %v", err)
	}
	return owner
}

// TestDeployDoesNotOverwriteAnotherTenantsDefinition is the property: after
// tenant B deploys a definition whose name tenant A already owns, A's bytes
// must still be A's.
func TestDeployDoesNotOverwriteAnotherTenantsDefinition(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			const (
				tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
				tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
			)

			tb, ok := backend.(tenantSetupBackend)
			if !ok {
				t.Fatalf("backend %T does not implement SetupForTenant; this test needs two tenants", backend)
			}

			storeA, teardownA := tb.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := tb.SetupForTenant(t, tenantB)
			defer teardownB()
			ctx := context.Background()

			// The kind of name two customers pick independently.
			const defName = "order-processor"
			aBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0xAA}
			bBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0xBB}

			if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: aBytes,
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("tenant A's own first deploy failed: %v", err)
			}

			// B's deploy of a name it does not own must not succeed silently.
			// Refusing is the fix; succeeding is the defect. Either way, what
			// the next assertion checks is A's bytes.
			errB := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: bBytes,
				ABIVersion: 1, MinVersion: 1,
			})
			if errB == nil {
				t.Errorf("tenant B deployed over tenant A's definition %q and was told it succeeded", defName)
			}

			gotA, err := storeA.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef(A): %v", err)
			}
			if gotA == nil {
				t.Fatalf("tenant A's definition %q is gone after tenant B deployed", defName)
			}
			if !bytes.Equal(gotA.WASMBytes, aBytes) {
				t.Errorf("tenant A's definition %q now carries %#v, not A's %#v -- "+
					"tenant B replaced the code A's workflows execute",
					defName, gotA.WASMBytes, aBytes)
			}

			// A must still be able to deploy over its own definition: this is
			// the ordinary redeploy path, and a guard that broke it would be
			// worse than the defect.
			newABytes := []byte{0x00, 0x61, 0x73, 0x6d, 0xA2}
			if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: newABytes,
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("tenant A can no longer redeploy its own definition: %v", err)
			}
			gotA, err = storeA.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef(A) after redeploy: %v", err)
			}
			if gotA == nil || !bytes.Equal(gotA.WASMBytes, newABytes) {
				t.Errorf("tenant A's redeploy did not take effect")
			}
		})
	}
}

// TestDeployRecordsTheDeployingTenant is the half of 3.12 that makes the guard
// above possible at all: a definition has to record who owns it.
//
// PostgresStore hardcoded the default tenant (store_deployment.go), ignoring
// s.tenantID, and MSSQLStore's MERGE did not name tenant_id in its INSERT
// column list, so it took the column default -- the same value. Only
// MySQLStore passed s.tenantID. With every definition owned by the default
// tenant, and PostgreSQL's policy on this table admitting the default tenant
// by design (`tenant_id = cleat.assert_tenant_set() OR tenant_id = '000…'`),
// every definition was a shared definition.
func TestDeployRecordsTheDeployingTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			const tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"

			tb, ok := backend.(tenantSetupBackend)
			if !ok {
				t.Fatalf("backend %T does not implement SetupForTenant", backend)
			}
			storeA, teardownA := tb.SetupForTenant(t, tenantA)
			defer teardownA()
			ctx := context.Background()

			const defName = "owner-recorded-def"
			if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// Read the column directly rather than through the store: the
			// point is what was written, and every read path here is already
			// scoped by the same tenant that wrote it.
			owner := readDefTenantID(t, backend, defName, 1)
			if owner != tenantA {
				t.Errorf("a definition deployed by tenant %s is owned by %q; "+
					"an unowned definition is one any tenant may overwrite",
					tenantA, owner)
			}
		})
	}
}
