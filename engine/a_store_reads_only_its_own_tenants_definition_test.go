package engine

// cleat#3740. The PostgreSQL statements that read a definition for a RUN -- LoadWASM,
// GetWASMLength, GetWASMDigest, LoadWorkflowConfig, LoadDAGSpec, ListVersions and
// ValidateVersion -- carried no tenant predicate: row-level security was the only scope. SQL
// Server and MySQL already carried `tenant_id = <the store's tenant>` on the same reads.
// Through a role that BYPASSES the policy (a superuser, or BYPASSRLS) a store therefore
// answered from whichever tenant's row the plan met first. cmd/cleat-worker's
// TestATenantsRunExecutesItsOwnDefinitionEvenThroughARoleThatBypassesRLS shows the consequence
// for a run; this file is one subtest per statement, so each can be shown red on its own.
//
// This runs as the connection the suite gives it, which is a superuser, so the statement is the
// only scope. The fixture is what makes it mean anything: two tenants hold the SAME (name,
// version) with different contents, and the rows differ in length, digest, history limit and
// DAG spec, so a statement with no predicate cannot answer correctly by luck. Each assertion
// runs in BOTH directions.

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

const (
	rdTenantA = DefaultTenantUUID
	rdTenantB = "f3740000-0000-4000-8000-0000000000bb"
	rdShared  = "rd-3740-shared" // both tenants hold version 1, with different contents
	rdOnlyB   = "rd-3740-only-b" // only tenant B holds it
)

var (
	rdBytesA = []byte{0x00, 0x61, 0x73, 0x6d, 0xaa}
	rdBytesB = []byte{0x00, 0x61, 0x73, 0x6d, 0xbb, 0xbb, 0xbb}
)

type rdFixture struct {
	db   *sql.DB
	a, b *PostgresStore
	ctx  context.Context
}

func newRDFixture(t *testing.T) *rdFixture {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })

	// PRECONDITION: the connection BYPASSES the policy. Without it a pass says nothing about
	// the statement, because the policy is then doing the scoping.
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("reading the test role: %v", err)
	}
	if !bypass {
		t.Fatal("this test needs a connection that BYPASSES row-level security (a superuser): the statement is its only scope there, " +
			"and on a connection the policy applies to it would pass without measuring the statement at all")
	}
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3740-b', false) ON CONFLICT (tenant_id) DO NOTHING`, rdTenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	cleanup := func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'rd-3740-%'`)
	}
	t.Cleanup(func() {
		cleanup()
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, rdTenantB)
	})
	cleanup()

	f := &rdFixture{
		db:  db,
		a:   NewPostgresStore(db).WithTenant(rdTenantA),
		b:   NewPostgresStore(db).WithTenant(rdTenantB),
		ctx: context.Background(),
	}
	deploy := func(st *PostgresStore, name string, version int, b []byte) {
		t.Helper()
		if err := st.DeployWorkflowDef(f.ctx, &WorkflowDef{Name: name, Version: version, WASMBytes: b, ABIVersion: 1, MinVersion: 1}); err != nil {
			t.Fatalf("DeployWorkflowDef %s v%d: %v", name, version, err)
		}
	}
	// Tenant B first, so a statement with no predicate that returns "the first row" answers B's.
	deploy(f.b, rdShared, 1, rdBytesB)
	deploy(f.b, rdShared, 3, rdBytesB)
	deploy(f.b, rdOnlyB, 1, rdBytesB)
	deploy(f.a, rdShared, 1, rdBytesA)
	deploy(f.a, rdShared, 2, rdBytesA)

	set := func(tenant string, maxHistory int, dag string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE workflow_defs SET max_history_length = $1, dag_spec = $2::jsonb WHERE tenant_id = $3 AND name = $4 AND version = 1`,
			maxHistory, dag, tenant, rdShared); err != nil {
			t.Fatalf("setting the history limit and DAG spec for tenant %s: %v", tenant[:8], err)
		}
	}
	set(rdTenantA, 111, `{"owner":"a"}`)
	set(rdTenantB, 222, `{"owner":"b"}`)
	return f
}

// both runs check once per tenant: own is the index of that tenant's row in the {A, B} pair.
func (f *rdFixture) both(t *testing.T, check func(t *testing.T, name string, st *PostgresStore, own int)) {
	t.Helper()
	check(t, "tenant A", f.a, 0)
	check(t, "tenant B", f.b, 1)
}

func TestAStoreReadsOnlyItsOwnTenantsDefinitionEvenThroughARoleThatBypassesRLS(t *testing.T) {
	// PREMISE, measured: the fixture is what it says, read with the tenant named in the SQL so
	// the reading cannot be wrong in the way the statements under test are.
	f := newRDFixture(t)
	for tenant, want := range map[string][]byte{rdTenantA: rdBytesA, rdTenantB: rdBytesB} {
		var got []byte
		if err := f.db.QueryRow(`SELECT wasm_bytes FROM workflow_defs WHERE tenant_id = $1 AND name = $2 AND version = 1`, tenant, rdShared).Scan(&got); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the premise of this test does not hold: tenant %s's %s v1 is %x (err=%v), want %x", tenant[:8], rdShared, got, err, want)
		}
	}

	bytesOf := [2][]byte{rdBytesA, rdBytesB}

	t.Run("LoadWASM returns the store's own tenant's bytes", func(t *testing.T) {
		f := newRDFixture(t)
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.LoadWASM(f.ctx, rdShared, 1)
			if err != nil {
				t.Fatalf("%s: LoadWASM: %v", name, err)
			}
			if !bytes.Equal(got, bytesOf[own]) {
				t.Errorf("%s's store loaded %x for %s v1, which is not its own build %x (cleat#3740)", name, got, rdShared, bytesOf[own])
			}
		})
		// A version only the other tenant holds is NOT FOUND, not the other tenant's bytes.
		if got, err := f.a.LoadWASM(f.ctx, rdOnlyB, 1); err == nil {
			t.Errorf("tenant A's store loaded %x of %s, which only tenant B holds (cleat#3740)", got, rdOnlyB)
		}
	})

	t.Run("GetWASMLength answers with the store's own tenant's length", func(t *testing.T) {
		f := newRDFixture(t)
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.GetWASMLength(f.ctx, rdShared, 1)
			if err != nil {
				t.Fatalf("%s: GetWASMLength: %v", name, err)
			}
			if got != int64(len(bytesOf[own])) {
				t.Errorf("%s's store reported length %d for %s v1, want its own %d (cleat#3740)", name, got, rdShared, len(bytesOf[own]))
			}
		})
		if got, err := f.a.GetWASMLength(f.ctx, rdOnlyB, 1); err == nil {
			t.Errorf("tenant A's store reported length %d of %s, which only tenant B holds (cleat#3740)", got, rdOnlyB)
		}
	})

	t.Run("GetWASMDigest answers with the store's own tenant's digest", func(t *testing.T) {
		f := newRDFixture(t)
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.GetWASMDigest(f.ctx, rdShared, 1)
			if err != nil {
				t.Fatalf("%s: GetWASMDigest: %v", name, err)
			}
			if want := WASMDigest(bytesOf[own]); got != want {
				t.Errorf("%s's store reported digest %s for %s v1, want its own %s (cleat#3740)", name, got, rdShared, want)
			}
		})
		if got, err := f.a.GetWASMDigest(f.ctx, rdOnlyB, 1); err == nil {
			t.Errorf("tenant A's store reported digest %s of %s, which only tenant B holds (cleat#3740)", got, rdOnlyB)
		}
	})

	t.Run("LoadWorkflowConfig returns the store's own tenant's history limit", func(t *testing.T) {
		f := newRDFixture(t)
		want := [2]int{111, 222}
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.LoadWorkflowConfig(f.ctx, rdShared, 1)
			if err != nil {
				t.Fatalf("%s: LoadWorkflowConfig: %v", name, err)
			}
			if got != want[own] {
				t.Errorf("%s's store read history limit %d for %s v1, want its own %d (cleat#3740)", name, got, rdShared, want[own])
			}
		})
		if got, err := f.a.LoadWorkflowConfig(f.ctx, rdOnlyB, 1); err == nil {
			t.Errorf("tenant A's store read history limit %d of %s, which only tenant B holds (cleat#3740)", got, rdOnlyB)
		}
	})

	t.Run("LoadDAGSpec returns the store's own tenant's spec", func(t *testing.T) {
		f := newRDFixture(t)
		want := [2]string{`{"owner": "a"}`, `{"owner": "b"}`} // jsonb normalises spacing
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.LoadDAGSpec(f.ctx, rdShared, 1)
			if err != nil {
				t.Fatalf("%s: LoadDAGSpec: %v", name, err)
			}
			if string(got) != want[own] {
				t.Errorf("%s's store read DAG spec %s for %s v1, want its own %s (cleat#3740)", name, got, rdShared, want[own])
			}
		})
		if got, err := f.a.LoadDAGSpec(f.ctx, rdOnlyB, 1); err == nil {
			t.Errorf("tenant A's store read DAG spec %s of %s, which only tenant B holds (cleat#3740)", got, rdOnlyB)
		}
	})

	t.Run("ListVersions lists only the store's own tenant's versions", func(t *testing.T) {
		f := newRDFixture(t)
		want := [2][]int{{2, 1}, {3, 1}}
		f.both(t, func(t *testing.T, name string, st *PostgresStore, own int) {
			got, err := st.ListVersions(f.ctx, rdShared)
			if err != nil {
				t.Fatalf("%s: ListVersions: %v", name, err)
			}
			if !reflect.DeepEqual(got, want[own]) {
				t.Errorf("%s's store listed versions %v of %s, want its own %v (cleat#3740)", name, got, rdShared, want[own])
			}
		})
		if got, err := f.a.ListVersions(f.ctx, rdOnlyB); err != nil || len(got) != 0 {
			t.Errorf("tenant A's store listed versions %v (err=%v) of %s, which only tenant B holds (cleat#3740)", got, err, rdOnlyB)
		}
	})

	t.Run("ValidateVersion is true only for a version the store's tenant holds", func(t *testing.T) {
		f := newRDFixture(t)
		// version 2 is A's alone, version 3 is B's alone.
		for _, c := range []struct {
			name    string
			st      *PostgresStore
			version int
			want    bool
		}{
			{"tenant A, its own v2", f.a, 2, true}, {"tenant A, B's v3", f.a, 3, false},
			{"tenant B, its own v3", f.b, 3, true}, {"tenant B, A's v2", f.b, 2, false},
		} {
			got, err := c.st.ValidateVersion(f.ctx, rdShared, c.version)
			if err != nil {
				t.Fatalf("%s: ValidateVersion: %v", c.name, err)
			}
			if got != c.want {
				t.Errorf("%s: ValidateVersion(%s, %d) = %v, want %v (cleat#3740)", c.name, rdShared, c.version, got, c.want)
			}
		}
	})
}
