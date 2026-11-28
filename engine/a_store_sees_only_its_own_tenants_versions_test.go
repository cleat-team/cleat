package engine

// cleat#3729. Every statement on the workflow-version path -- list, get, deprecate, purge,
// the purge hold, the active-instance counts -- reached workflow_defs and workflow_leases on
// PostgreSQL with NO tenant predicate; row-level security was the only scope. SQL Server and
// MySQL already carried `tenant_id = <the store's tenant>` on the same statements. Through a
// role that BYPASSES the policy (a superuser, or BYPASSRLS) a PostgreSQL store therefore saw
// every tenant's rows, and GarbageCollectVersions, which groups by workflow name, treated two
// tenants' same-named workflows as one: measured, a pass through the default tenant's store
// deleted tenant B's versions [1 2] and kept the default tenant's [3 4].
//
// This runs as the connection the suite gives it, which is a superuser, so the policy does
// not apply and the statement is the only scope. The fixture is what makes it mean anything:
// two tenants hold the SAME (name, version) with different contents, so a statement with no
// predicate answers from whichever row it meets first, and a test with one tenant, or with
// distinct names, would pass against the bug.
//
// One subtest per statement so each can be shown red on its own, and each asserts BOTH
// directions (tenant A's store must not touch B, and B's must not touch A).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

const (
	gcTenantA  = DefaultTenantUUID
	gcTenantB  = "f3729000-0000-4000-8000-00000000000b"
	gcShared   = "gc-3729-shared" // B holds versions 1 and 2, A holds 3 and 4
	gcSameName = "gc-3729-same"   // BOTH hold versions 1 and 2, with different ABI versions
	gcABIA     = 11
	gcABIB     = 22
)

type gcFixture struct {
	db     *sql.DB
	a, b   *PostgresStore
	ctx    context.Context
	runOnB string // an active run on gcSameName v2, in tenant B only
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })

	// PRECONDITION: the connection BYPASSES the policy. Without it a pass says nothing
	// about the statement, because the policy is then doing the scoping.
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("reading the test role: %v", err)
	}
	if !bypass {
		t.Fatal("this test needs a connection that BYPASSES row-level security (a superuser): the statement is its only scope there, " +
			"and on a connection the policy applies to it would pass without measuring the statement at all")
	}

	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3729-b', false) ON CONFLICT (tenant_id) DO NOTHING`, gcTenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'gc-3729-%'`)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, gcTenantB)
	})
	_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'gc-3729-%'`)

	f := &gcFixture{
		db:  db,
		a:   NewPostgresStore(db).WithTenant(gcTenantA),
		b:   NewPostgresStore(db).WithTenant(gcTenantB),
		ctx: context.Background(),
	}
	deploy := func(st *PostgresStore, name string, version, abi int) {
		t.Helper()
		if err := st.DeployWorkflowDef(f.ctx, &WorkflowDef{
			Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: abi, MinVersion: 1, GCEligible: true,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef %s v%d: %v", name, version, err)
		}
	}
	// Tenant B first, so a statement with no predicate that returns "the first row" answers B's.
	deploy(f.b, gcShared, 1, gcABIB)
	deploy(f.b, gcShared, 2, gcABIB)
	deploy(f.b, gcSameName, 1, gcABIB)
	deploy(f.b, gcSameName, 2, gcABIB)
	deploy(f.a, gcShared, 3, gcABIA)
	deploy(f.a, gcShared, 4, gcABIA)
	deploy(f.a, gcSameName, 1, gcABIA)
	deploy(f.a, gcSameName, 2, gcABIA)

	// A run still executing on gcSameName v2 in tenant B only.
	f.runOnB = fmt.Sprintf("gc-3729-run-%d", time.Now().UnixNano())
	if _, _, err := f.b.StartNewRun(f.ctx, f.runOnB, gcSameName, 2, json.RawMessage(`{}`), "", gcTenantB, 0); err != nil {
		t.Fatalf("starting a run on tenant B: %v", err)
	}
	return f
}

// held lists the versions of name that tenant holds, read by the test itself with the tenant
// named in the SQL, so the reading cannot be wrong in the same way the statement under test is.
func (f *gcFixture) held(t *testing.T, tenant, name string) []int {
	t.Helper()
	rows, err := f.db.Query(`SELECT version FROM workflow_defs WHERE tenant_id = $1 AND name = $2 ORDER BY version`, tenant, name)
	if err != nil {
		t.Fatalf("listing versions: %v", err)
	}
	defer rows.Close()
	var vs []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	return vs
}

func TestAStoreSeesOnlyItsOwnTenantsVersionsEvenThroughARoleThatBypassesRLS(t *testing.T) {
	f := newGCFixture(t)

	// PREMISE, measured: the fixture is what it says.
	for _, c := range []struct {
		tenant, name string
		want         []int
	}{
		{gcTenantB, gcShared, []int{1, 2}}, {gcTenantA, gcShared, []int{3, 4}},
		{gcTenantB, gcSameName, []int{1, 2}}, {gcTenantA, gcSameName, []int{1, 2}},
	} {
		if got := f.held(t, c.tenant, c.name); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("tenant %s holds %v of %s, want %v: the premise of this test does not hold", c.tenant[:8], got, c.name, c.want)
		}
	}

	t.Run("GarbageCollectVersions: A's pass leaves B's versions, and B's leaves A's", func(t *testing.T) {
		f := newGCFixture(t)
		opts := GCOptions{MinVersionsToKeep: 2, MaxVersionAge: time.Millisecond}
		time.Sleep(20 * time.Millisecond)
		if _, err := GarbageCollectVersions(f.ctx, f.a, opts); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, gcTenantB, gcShared); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("after a pass through tenant A's store tenant B holds %v of %s, want [1 2]: the pass treated two "+
				"tenants' same-named workflows as one and deleted another tenant's versions (cleat#3729)", got, gcShared)
		}
		if got := f.held(t, gcTenantA, gcShared); !reflect.DeepEqual(got, []int{3, 4}) {
			t.Errorf("tenant A holds %v of %s after its own pass, want [3 4]", got, gcShared)
		}
		if _, err := GarbageCollectVersions(f.ctx, f.b, opts); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, gcTenantA, gcShared); !reflect.DeepEqual(got, []int{3, 4}) {
			t.Errorf("after a pass through tenant B's store tenant A holds %v of %s, want [3 4] (cleat#3729)", got, gcShared)
		}
		if got := f.held(t, gcTenantB, gcShared); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("tenant B holds %v of %s after its own pass, want [1 2]", got, gcShared)
		}
	})

	t.Run("ListWorkflowDefs returns only the store's tenant's rows", func(t *testing.T) {
		f := newGCFixture(t)
		for name, st := range map[string]*PostgresStore{gcTenantA: f.a, gcTenantB: f.b} {
			want := gcABIA
			if name == gcTenantB {
				want = gcABIB
			}
			for _, listName := range []string{"", gcSameName} {
				defs, err := st.ListWorkflowDefs(f.ctx, listName)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range defs {
					if d.ABIVersion != want {
						t.Errorf("tenant %s's store listed %s v%d with ABI %d, which is the OTHER tenant's row (cleat#3729)", name[:8], d.Name, d.Version, d.ABIVersion)
					}
				}
				wantN := map[string]int{"": 4, gcSameName: 2}[listName]
				if len(defs) != wantN {
					t.Errorf("tenant %s's store listed %d rows for name %q, want %d", name[:8], len(defs), listName, wantN)
				}
			}
		}
	})

	t.Run("GetWorkflowDef answers with the store's tenant's row", func(t *testing.T) {
		f := newGCFixture(t)
		for tenant, st := range map[string]*PostgresStore{gcTenantA: f.a, gcTenantB: f.b} {
			def, err := st.GetWorkflowDef(f.ctx, gcSameName, 2)
			if err != nil || def == nil {
				t.Fatalf("GetWorkflowDef: %v %v", def, err)
			}
			want := map[string]int{gcTenantA: gcABIA, gcTenantB: gcABIB}[tenant]
			if def.ABIVersion != want {
				t.Errorf("tenant %s's store got ABI %d for %s v2, want %d: it answered with the other tenant's row (cleat#3729)", tenant[:8], def.ABIVersion, gcSameName, want)
			}
		}
	})

	t.Run("MarkVersionDeprecated changes only the store's tenant's row", func(t *testing.T) {
		f := newGCFixture(t)
		if err := f.a.MarkVersionDeprecated(f.ctx, gcSameName, 1, true); err != nil {
			t.Fatal(err)
		}
		var depA, depB sql.NullTime
		_ = f.db.QueryRow(`SELECT disabled_at FROM workflow_defs WHERE tenant_id = $1 AND name = $2 AND version = 1`, gcTenantA, gcSameName).Scan(&depA)
		_ = f.db.QueryRow(`SELECT disabled_at FROM workflow_defs WHERE tenant_id = $1 AND name = $2 AND version = 1`, gcTenantB, gcSameName).Scan(&depB)
		if !depA.Valid {
			t.Error("tenant A's own version was not deprecated")
		}
		if depB.Valid {
			t.Error("deprecating a version through tenant A's store also deprecated tenant B's same-numbered version (cleat#3729)")
		}
	})

	t.Run("PurgeWorkflowDef deletes only the store's tenant's row", func(t *testing.T) {
		f := newGCFixture(t)
		if err := f.a.PurgeWorkflowDef(f.ctx, gcSameName, 1); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, gcTenantA, gcSameName); !reflect.DeepEqual(got, []int{2}) {
			t.Errorf("tenant A holds %v after purging its v1, want [2]", got)
		}
		if got := f.held(t, gcTenantB, gcSameName); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("tenant B holds %v of %s after tenant A purged v1, want [1 2]: the purge deleted every tenant's v1 (cleat#3729)", got, gcSameName)
		}
	})

	t.Run("the purge hold (the sharded path's DELETE) deletes only the store's tenant's row", func(t *testing.T) {
		f := newGCFixture(t)
		h, err := f.a.beginWorkflowDefPurge(f.ctx, gcSameName, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.commit(); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, gcTenantB, gcSameName); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("tenant B holds %v of %s after tenant A's purge hold committed, want [1 2] (cleat#3729)", got, gcSameName)
		}
		if got := f.held(t, gcTenantA, gcSameName); !reflect.DeepEqual(got, []int{2}) {
			t.Errorf("tenant A holds %v after its own purge hold, want [2]", got)
		}
	})

	t.Run("the active-instance counts are the store's tenant's", func(t *testing.T) {
		f := newGCFixture(t)
		// A run is active on v2 in tenant B only.
		if n, err := f.b.CountActiveInstances(f.ctx, gcSameName, 2); err != nil || n != 1 {
			t.Errorf("tenant B's own count of %s v2 = %d (err %v), want 1", gcSameName, n, err)
		}
		if n, err := f.a.CountActiveInstances(f.ctx, gcSameName, 2); err != nil || n != 0 {
			t.Errorf("tenant A's count of %s v2 = %d (err %v), want 0: tenant B's running instance was counted as tenant A's, "+
				"so it would protect A's same-numbered version from collection (cleat#3729)", gcSameName, n, err)
		}
		countsA, err := f.a.GetActiveInstanceCountsByVersion(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n := countsA[gcSameName+":2"]; n != 0 {
			t.Errorf("tenant A's active-instance counts include %d for %s:2, want none (cleat#3729)", n, gcSameName)
		}
		countsB, err := f.b.GetActiveInstanceCountsByVersion(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n := countsB[gcSameName+":2"]; n != 1 {
			t.Errorf("tenant B's own active-instance counts have %d for %s:2, want 1", n, gcSameName)
		}
	})

	// The three call sites of GarbageCollectVersions are the worker's version-GC loop
	// (cmd/cleat-worker/setup.go runVersionGCSweep), `cleatctl versions gc`
	// (cmd/cleatctl/versions.go) and this handler. They all reach the database only
	// through the store methods above, so each has its own test through its own entry
	// point as well: the loop and cleatctl in their packages, the handler here.
	t.Run("POST /api/versions/gc through a bypassing store leaves the other tenant's versions", func(t *testing.T) {
		f := newGCFixture(t)
		mux := http.NewServeMux()
		RegisterVersionHandler(mux, StaticVersionStore(f.a))
		time.Sleep(20 * time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, "/api/versions/gc?min_versions=2&max_age=1ms", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /api/versions/gc = %d: %s", rec.Code, rec.Body.String())
		}
		if got := f.held(t, gcTenantB, gcShared); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("after POST /api/versions/gc as tenant A tenant B holds %v of %s, want [1 2] (cleat#3729)", got, gcShared)
		}
	})
}
