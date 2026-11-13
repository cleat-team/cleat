package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3740, SQL Server. On PostgreSQL the reads a run makes of its definition (LoadWASM,
// GetWASMLength, LoadWorkflowConfig, LoadDAGSpec, ListVersions) carried no tenant predicate. The SQL
// Server statements already carry `tenant_id = <the store's tenant>`, so this was expected to be green
// before the PostgreSQL change, and it is the measurement that says so: two tenants hold the SAME
// (name, version) with different contents, and each store must read only its own.
//
// It runs as TWO principals, because they hold the scope in different places:
//
//   - `sa`, a sysadmin mapped to dbo. SQL Server's SESSION_CONTEXT policy applies to it too, so no
//     principal bypasses the policy; this is the strongest login there is.
//   - a login in cleat_app_role, which is what the shipped worker connects as.
//
// Each statement already carries the predicate, so both are expected to pass. THIS IS A PIN ON THE
// OUTCOME, NOT ON THE PREDICATE: with the predicate removed from LoadWASM and ListVersions the test stays
// green for both principals (measured), because the SESSION_CONTEXT policy applies to every login and is
// what scopes the read; the predicate is the second layer. Not measured: MySQL (tenant-per-database, read
// only), and a principal outside cleat_app_role.
func TestASQLServerStoreReadsOnlyItsOwnTenantsDefinition(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3740 test")
	}
	const other = "ffffffff-0000-4000-8000-000037400001"
	rig := mssqlHBRig(t, other)
	rig.register(t, other)
	ctx := context.Background()

	// The application login gets its own factory, so its stores run on ITS pools.
	u := mssqlDatabaseURL(t, rig.db)
	mssqlAppLoginURL(t, rig.db, u, false)
	appFactory := engine.NewMSSQLStoreFactory(u.String())
	t.Cleanup(func() { _ = appFactory.Close() })

	// PRECONDITION: the two principals are what they say they are.
	principal := func(db *sql.DB) (sysadmin, dbOwner int) {
		t.Helper()
		if err := db.QueryRow(`SELECT IS_SRVROLEMEMBER('sysadmin'), IS_MEMBER('db_owner')`).Scan(&sysadmin, &dbOwner); err != nil {
			t.Fatalf("UNMEASURED: reading the principal's roles: %v", err)
		}
		return
	}
	if s, _ := principal(rig.db); s != 1 {
		t.Fatalf("UNMEASURED: the rig's admin connection is not a sysadmin (%d)", s)
	}
	appDB, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("UNMEASURED: opening the application login: %v", err)
	}
	t.Cleanup(func() { _ = appDB.Close() })
	if s, o := principal(appDB); s != 0 || o != 0 {
		t.Fatalf("the application login is a sysadmin=%d db_owner=%d: this arm would measure a privileged principal, not a limited one", s, o)
	}

	const shared, onlyB = "rd-3740-sql-shared", "rd-3740-sql-only-b"
	bytesA := []byte{0x00, 0x61, 0x73, 0x6d, 0xaa}
	bytesB := []byte{0x00, 0x61, 0x73, 0x6d, 0xbb, 0xbb, 0xbb}

	for _, p := range []struct {
		name string
		open func(tenant string) (engine.WorkflowStore, func())
	}{
		{"sa", func(tenant string) (engine.WorkflowStore, func()) {
			st, c, err := rig.factory.OpenStore(ctx, tenant, "default")
			if err != nil {
				t.Fatalf("opening a store for %s: %v", tenant, err)
			}
			return st, func() { _ = c.Close() }
		}},
		{"the application login (cleat_app_role)", func(tenant string) (engine.WorkflowStore, func()) {
			st, c, err := appFactory.OpenStore(ctx, tenant, "default")
			if err != nil {
				t.Fatalf("opening a store for %s as the application login: %v", tenant, err)
			}
			return st, func() { _ = c.Close() }
		}},
	} {
		t.Run(p.name, func(t *testing.T) {
			a, closeA := p.open(engine.DefaultTenantUUID)
			defer closeA()
			b, closeB := p.open(other)
			defer closeB()
			name := fmt.Sprintf("%s-%s", shared, map[string]string{"sa": "sa", "the application login (cleat_app_role)": "app"}[p.name])
			only := fmt.Sprintf("%s-%s", onlyB, map[string]string{"sa": "sa", "the application login (cleat_app_role)": "app"}[p.name])
			t.Cleanup(func() {
				_, _ = rig.db.Exec(`DELETE FROM workflow_defs WHERE name IN (@p1, @p2)`, name, only)
			})
			deploy := func(st engine.WorkflowStore, n string, version int, bs []byte) {
				t.Helper()
				if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{Name: n, Version: version, WASMBytes: bs, ABIVersion: 1, MinVersion: 1}); err != nil {
					t.Fatalf("DeployWorkflowDef %s v%d: %v", n, version, err)
				}
			}
			// Tenant B first, so a statement with no predicate that returns "the first row" answers B's.
			deploy(b, name, 1, bytesB)
			deploy(b, name, 3, bytesB)
			deploy(b, only, 1, bytesB)
			deploy(a, name, 1, bytesA)
			deploy(a, name, 2, bytesA)
			// SQL Server's policy applies to `sa` too, so the tenant is set on one pinned connection, and a
			// statement that matched no row would change nothing and say nothing: the row count is checked.
			setHistory := func(tenant string, limit int) {
				t.Helper()
				conn, err := rig.db.Conn(ctx)
				if err != nil {
					t.Fatalf("UNMEASURED: %v", err)
				}
				defer conn.Close()
				if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
					t.Fatalf("UNMEASURED: scoping the connection to %s: %v", tenant, err)
				}
				res, err := conn.ExecContext(ctx, `UPDATE workflow_defs SET max_history_length = @p1 WHERE tenant_id = @p2 AND name = @p3 AND version = 1`, limit, tenant, name)
				if err != nil {
					t.Fatalf("UNMEASURED: setting the history limit for %s: %v", tenant, err)
				}
				if n, _ := res.RowsAffected(); n != 1 {
					t.Fatalf("UNMEASURED: setting the history limit for %s changed %d row(s), want 1", tenant, n)
				}
			}
			setHistory(engine.DefaultTenantUUID, 111)
			setHistory(other, 222)

			for _, c := range []struct {
				tenant   string
				st       engine.WorkflowStore
				bs       []byte
				history  int
				versions []int
			}{
				{"tenant A", a, bytesA, 111, []int{2, 1}},
				{"tenant B", b, bytesB, 222, []int{3, 1}},
			} {
				got, err := c.st.LoadWASM(ctx, name, 1)
				if err != nil || !bytes.Equal(got, c.bs) {
					t.Errorf("%s's store loaded %x (err=%v) for %s v1, want its own %x", c.tenant, got, err, name, c.bs)
				}
				if n, err := c.st.GetWASMLength(ctx, name, 1); err != nil || n != int64(len(c.bs)) {
					t.Errorf("%s's store reported length %d (err=%v), want its own %d", c.tenant, n, err, len(c.bs))
				}
				if h, err := c.st.LoadWorkflowConfig(ctx, name, 1); err != nil || h != c.history {
					t.Errorf("%s's store read history limit %d (err=%v), want its own %d", c.tenant, h, err, c.history)
				}
				if vs, err := c.st.ListVersions(ctx, name); err != nil || !reflect.DeepEqual(vs, c.versions) {
					t.Errorf("%s's store listed %v (err=%v), want its own %v", c.tenant, vs, err, c.versions)
				}
			}
			// A version only the other tenant holds is not found, not the other tenant's bytes.
			if got, err := a.LoadWASM(ctx, only, 1); err == nil {
				t.Errorf("tenant A's store loaded %x of %s, which only tenant B holds", got, only)
			}
			if vs, err := a.ListVersions(ctx, only); err != nil || len(vs) != 0 {
				t.Errorf("tenant A's store listed %v (err=%v) of %s, which only tenant B holds", vs, err, only)
			}
		})
	}
}
