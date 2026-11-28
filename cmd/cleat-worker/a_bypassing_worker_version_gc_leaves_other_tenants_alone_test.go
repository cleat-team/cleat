package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3729, the worker loop's call site. runVersionGCSweep collects through the worker's
// own store, and on PostgreSQL the statements behind it had no tenant predicate, so through a
// role that bypasses row-level security the pass saw every tenant's definitions and treated
// two tenants' same-named workflows as one: it deleted tenant B's versions 1 and 2 and kept
// the worker's own tenant's 3 and 4. The store-level half, one subtest per statement, is
// engine/a_store_sees_only_its_own_tenants_versions_test.go.
//
// The connection is the superuser twin the other RLS tests use (the policy does not apply),
// so the statement is the only scope; on a connection the policy applies to this would pass
// whatever the statements said, so that is asserted.
func TestAWorkersVersionGCLeavesAnotherTenantsVersionsAloneThroughABypassingRole(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_versiongc3729")
	var bypass bool
	if err := owner.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil || !bypass {
		t.Fatalf("this test needs a role that bypasses row-level security (err %v, bypass %v)", err, bypass)
	}
	const other = "dddddddd-0000-4000-8000-000037290001"
	rig := postgresHBRig(t, owner, owner, false, other)
	rig.register(t, other)
	ctx := context.Background()
	const name = "gc-3729-worker-shared"
	_, _ = owner.Exec(`DELETE FROM workflow_defs WHERE name = $1`, name)
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM workflow_defs WHERE name = $1`, name) })

	deploy := func(tenant string, version int) {
		t.Helper()
		st, closer, err := rig.factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1, GCEligible: true,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef v%d for %s: %v", version, tenant, err)
		}
	}
	deploy(other, 1)
	deploy(other, 2)
	deploy(engine.DefaultTenantUUID, 3)
	deploy(engine.DefaultTenantUUID, 4)
	held := func(tenant string) []int {
		t.Helper()
		rows, err := owner.Query(`SELECT version FROM workflow_defs WHERE tenant_id = $1 AND name = $2 ORDER BY version`, tenant, name)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var vs []int
		for rows.Next() {
			var v int
			_ = rows.Scan(&v)
			vs = append(vs, v)
		}
		return vs
	}
	if got := held(other); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("tenant B holds %v before the sweep, want [1 2]: the premise does not hold", got)
	}

	w := newTestWorkerFromStore(rig.ownStore)
	defer w.cancel()
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true
	w.versionGCMinVersions = 2
	w.versionGCMaxAge = time.Millisecond
	time.Sleep(20 * time.Millisecond)

	w.runVersionGCSweep()

	if got := held(other); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("after the worker's version-GC pass tenant B holds %v of %s, want [1 2]: the pass ran through a role that "+
			"bypasses the policy and deleted another tenant's versions (cleat#3729)", got, name)
	}
	if got := held(engine.DefaultTenantUUID); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("the worker's own tenant holds %v of %s after its own pass, want [3 4]", got, name)
	}
}
