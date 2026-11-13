package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3729, SQL Server. On PostgreSQL the statements behind version GC had no tenant
// predicate and a role that bypasses row-level security saw every tenant's definitions. SQL
// Server (and MySQL) already carried `tenant_id = <the store's tenant>` on every one of those
// statements, so this was expected to be green BEFORE the change, and it is the measurement
// that says so rather than a belief: two tenants hold the SAME (name, version) with different
// contents, and each store must see, change, count and delete only its own.
//
// The connection is the rig's `sa`, a sysadmin login mapped to dbo, the strongest principal there is, so
// "no principal bypasses the policy" is not what carries this: the predicates are. NOT
// measured: a limited application login.
func TestASQLServerStoreSeesOnlyItsOwnTenantsVersions(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3729 test")
	}
	const other = "ffffffff-0000-4000-8000-000037290001"
	rig := mssqlHBRig(t, other)
	rig.register(t, other)
	ctx := context.Background()
	a := rig.ownStore
	b, closeB, err := rig.factory.OpenStore(ctx, other, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer closeB.Close()

	const shared, same = "gc-3729-sql-shared", "gc-3729-sql-same"
	const abiA, abiB = 11, 22
	deploy := func(st engine.WorkflowStore, name string, version, abi int) {
		t.Helper()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: abi, MinVersion: 1, GCEligible: true,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef %s v%d: %v", name, version, err)
		}
	}
	deploy(b, shared, 1, abiB)
	deploy(b, shared, 2, abiB)
	deploy(b, same, 1, abiB)
	deploy(b, same, 2, abiB)
	deploy(a, shared, 3, abiA)
	deploy(a, shared, 4, abiA)
	deploy(a, same, 1, abiA)
	deploy(a, same, 2, abiA)
	runB := fmt.Sprintf("gc-3729-sql-run-%d", time.Now().UnixNano())
	if _, _, err := b.StartNewRun(ctx, runB, same, 2, json.RawMessage(`{}`), "", other, 0); err != nil {
		t.Fatalf("starting a run on tenant B: %v", err)
	}

	// Read as the tenant the row belongs to: SQL Server's policy applies to every login.
	asTenant := func(tenant string, fn func(c *sql.Conn)) {
		t.Helper()
		c, err := rig.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
			t.Fatal(err)
		}
		fn(c)
	}
	held := func(tenant, name string) []int {
		t.Helper()
		var vs []int
		asTenant(tenant, func(c *sql.Conn) {
			rows, err := c.QueryContext(ctx, `SELECT version FROM workflow_defs WHERE tenant_id = @p1 AND name = @p2 ORDER BY version`, tenant, name)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var v int
				_ = rows.Scan(&v)
				vs = append(vs, v)
			}
		})
		return vs
	}
	disabled := func(tenant, name string, version int) bool {
		t.Helper()
		var at sql.NullTime
		asTenant(tenant, func(c *sql.Conn) {
			_ = c.QueryRowContext(ctx, `SELECT disabled_at FROM workflow_defs WHERE tenant_id = @p1 AND name = @p2 AND version = @p3`, tenant, name, version).Scan(&at)
		})
		return at.Valid
	}
	// PREMISE, measured.
	for _, c := range []struct {
		tenant, name string
		want         []int
	}{{other, shared, []int{1, 2}}, {engine.DefaultTenantUUID, shared, []int{3, 4}}, {other, same, []int{1, 2}}, {engine.DefaultTenantUUID, same, []int{1, 2}}} {
		if got := held(c.tenant, c.name); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("tenant %s holds %v of %s, want %v: the premise does not hold", c.tenant[:8], got, c.name, c.want)
		}
	}

	// list and get
	for tenant, st := range map[string]engine.WorkflowStore{engine.DefaultTenantUUID: a, other: b} {
		want := map[string]int{engine.DefaultTenantUUID: abiA, other: abiB}[tenant]
		defs, err := st.ListWorkflowDefs(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if d.ABIVersion != want {
				t.Errorf("tenant %s's store listed %s v%d with ABI %d, the other tenant's row", tenant[:8], d.Name, d.Version, d.ABIVersion)
			}
		}
		def, err := st.GetWorkflowDef(ctx, same, 2)
		if err != nil || def == nil || def.ABIVersion != want {
			t.Errorf("tenant %s's store got %+v (err %v) for %s v2, want ABI %d", tenant[:8], def, err, same, want)
		}
	}
	// counts: the run is B's only
	if n, err := a.CountActiveInstances(ctx, same, 2); err != nil || n != 0 {
		t.Errorf("tenant A's count of %s v2 = %d (err %v), want 0", same, n, err)
	}
	if n, err := b.CountActiveInstances(ctx, same, 2); err != nil || n != 1 {
		t.Errorf("tenant B's count of %s v2 = %d (err %v), want 1", same, n, err)
	}
	if counts, err := a.GetActiveInstanceCountsByVersion(ctx); err != nil || counts[same+":2"] != 0 {
		t.Errorf("tenant A's active counts include %v (err %v), want none for %s:2", counts, err, same)
	}
	// deprecate and purge through A leave B's same-numbered row
	if err := a.MarkVersionDeprecated(ctx, same, 1, true); err != nil {
		t.Fatal(err)
	}
	if !disabled(engine.DefaultTenantUUID, same, 1) || disabled(other, same, 1) {
		t.Errorf("deprecating through tenant A: A's row disabled=%v (want true), B's disabled=%v (want false)",
			disabled(engine.DefaultTenantUUID, same, 1), disabled(other, same, 1))
	}
	if err := a.PurgeWorkflowDef(ctx, same, 1); err != nil {
		t.Fatal(err)
	}
	if got := held(other, same); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("tenant B holds %v of %s after tenant A purged v1, want [1 2]", got, same)
	}
	// the collection itself, through A's store: B's [1 2] survive and A's [3 4] stay
	time.Sleep(20 * time.Millisecond)
	if _, err := engine.GarbageCollectVersions(ctx, a, engine.GCOptions{MinVersionsToKeep: 2, MaxVersionAge: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if got := held(other, shared); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("after a pass through tenant A's store tenant B holds %v of %s, want [1 2]", got, shared)
	}
	if got := held(engine.DefaultTenantUUID, shared); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("tenant A holds %v of %s after its own pass, want [3 4]", got, shared)
	}
}
