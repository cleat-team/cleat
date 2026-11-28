package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3729, cleatctl's call site. `cleatctl versions gc` runs GarbageCollectVersions
// through a store opened from --db, and an operator's DSN is the connection most likely to
// bypass row-level security. On PostgreSQL the statements behind the pass had no tenant
// predicate, so through such a role one pass treated two tenants' same-named workflows as one
// and deleted tenant B's versions 1 and 2 while keeping the default tenant's 3 and 4.
//
// The connection here is the suite's (a superuser, so the policy does not apply and the
// statement is the only scope); that is asserted rather than assumed. The store-level half is
// engine/a_store_sees_only_its_own_tenants_versions_test.go.
func TestVersionsGCThroughCleatctlLeavesAnotherTenantsVersionsAlone(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil || !bypass {
		t.Fatalf("this test needs a role that bypasses row-level security (err %v, bypass %v)", err, bypass)
	}
	const other = "eeeeeeee-0000-4000-8000-000037290001"
	const name = "gc-3729-cleatctl-shared"
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3729-ctl', false) ON CONFLICT (tenant_id) DO NOTHING`, other); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, name)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, name)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, other)
	})

	ctx := context.Background()
	deploy := func(tenant string, version int) {
		t.Helper()
		st := engine.NewPostgresStore(db).WithTenant(tenant)
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1, GCEligible: true,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef v%d for %s: %v", version, tenant, err)
		}
	}
	deploy(other, 1)
	deploy(other, 2)
	deploy(defaultTenantID, 3)
	deploy(defaultTenantID, 4)
	held := func(tenant string) []int {
		t.Helper()
		rows, err := db.Query(`SELECT version FROM workflow_defs WHERE tenant_id = $1 AND name = $2 ORDER BY version`, tenant, name)
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
		t.Fatalf("tenant B holds %v before the pass, want [1 2]: the premise does not hold", got)
	}
	time.Sleep(20 * time.Millisecond)

	// The store cleatctl opens: the default tenant's, on the --db connection.
	store := engine.NewPostgresStore(db).WithTenant(defaultTenantID)
	captureStdout(t, func() {
		runVersions(ctx, store, []string{"gc", "--min-versions=2", "--max-age=1ms"})
	})

	if got := held(other); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("after `cleatctl versions gc` tenant B holds %v of %s, want [1 2]: the pass ran through a role that bypasses "+
			"the policy and deleted another tenant's versions (cleat#3729)", got, name)
	}
	if got := held(defaultTenantID); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("the default tenant holds %v of %s after its own pass, want [3 4]", got, name)
	}
}
