package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The two real-database rigs for the workflow-library tests (cleat#3540, cleat#3629): PostgreSQL
// reached as a role row-level security applies to, and SQL Server reached as a tenant login. They
// lived inside TestAnInstallCopiesEveryColumnOfItsMembers, where nothing else could call them; they
// are here so every library test builds the same rig and none keeps a private copy.

// newPostgresLibRig is the PostgreSQL rig: the admin connection for setup and counting, and a store
// per tenant over the RLS role.
func newPostgresLibRig(t *testing.T) *libInstallRig {
	t.Helper()
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	t.Cleanup(func() { appDB.Close() })
	assertNotSuperuserBypass(t, appDB)
	ph := func(q string) string { // ? -> $1, $2, ...
		var b strings.Builder
		n := 0
		for _, r := range q {
			if r == '?' {
				n++
				fmt.Fprintf(&b, "$%d", n)
			} else {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return &libInstallRig{
		admin:   adminDB,
		dialect: DialectPostgres,
		name:    "postgres",
		schema:  "public",
		text:    func(col string) string { return col + "::text" },
		store: func(tenant string) installableStore {
			return NewPostgresStore(appDB).WithTenant(tenant)
		},
		exec: func(t *testing.T, _ string, q string, args ...any) {
			t.Helper()
			if _, err := adminDB.Exec(ph(q), args...); err != nil {
				t.Fatalf("exec %q: %v", q, err)
			}
		},
		query: func(t *testing.T, _ string, q string, args ...any) []map[string]string {
			t.Helper()
			return scanStringRows(t, adminDB, ph(q), args...)
		},
		seedMember: func(t *testing.T, library string, m libMember) {
			t.Helper()
			if _, err := adminDB.Exec(`
				INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version, min_version,
					plugin_deps, entry_points, task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::text[], $11, $12, $13::jsonb, $14::jsonb, $15, $16)`,
				library, m.name, m.version, m.wasm(), m.digest(), len(m.wasm()), m.abi(), m.minVersion(),
				m.pluginDeps(), fmt.Sprintf("{alpha%d,beta%d}", m.variant, m.variant), m.taskQueue(), m.maxHistory(),
				m.dagSpec(), m.entrySchemas(), m.exposure, m.validationDisabled()); err != nil {
				t.Fatalf("publish member %s: %v", m.name, err)
			}
		},
		seedStale: func(t *testing.T, tenant, library string, m libMember) {
			t.Helper()
			stale := staleOf(m)
			if _, err := adminDB.Exec(`
				INSERT INTO workflow_defs (tenant_id, name, version, library, abi_version, min_version, plugin_deps, entry_points,
					task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
				VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::text[], $9, $10, $11::jsonb, $12::jsonb, $13, $14)`,
				tenant, m.name, m.version, library, stale.abi(), stale.minVersion(), stale.pluginDeps(),
				fmt.Sprintf("{alpha%d,beta%d}", stale.variant, stale.variant), stale.taskQueue(), stale.maxHistory(),
				stale.dagSpec(), stale.entrySchemas(), stale.exposure, false); err != nil {
				t.Fatalf("seed a stale row for %s: %v", tenant, err)
			}
		},
		registerPlugins: func(t *testing.T, names ...string) {
			t.Helper()
			if err := recordPluginsOn(context.Background(), adminDB, DialectPostgres, names); err != nil {
				t.Fatalf("register plugins %v: %v", names, err)
			}
		},
		forgetPlugin: func(t *testing.T, name string) {
			t.Helper()
			if _, err := adminDB.Exec(`DELETE FROM admin.registered_plugins WHERE plugin_name = $1`, name); err != nil {
				t.Fatalf("forget plugin %q: %v", name, err)
			}
		},
		cleanup: func(tenants []string, library string) {
			for _, tenant := range tenants {
				_, _ = adminDB.Exec(`DELETE FROM tenant_library_installs WHERE tenant_id = $1`, tenant)
				_, _ = adminDB.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
			}
			_, _ = adminDB.Exec(`DELETE FROM library.bundle_members WHERE library = $1`, library)
			_, _ = adminDB.Exec(`DELETE FROM library.bundles WHERE library = $1`, library)
			_, _ = adminDB.Exec(`DELETE FROM library.members WHERE library = $1`, library)
		},
	}
}

// newMSSQLLibRig is the SQL Server rig.
func newMSSQLLibRig(t *testing.T) *libInstallRig {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	t.Cleanup(func() { db.Close() })
	testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
	ctx := context.Background()
	asTenant := func(t *testing.T, tenant string, fn func(c *sql.Conn)) {
		t.Helper()
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if tenant != "" {
			if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
				t.Fatalf("scoping the connection to %s: %v", tenant, err)
			}
		}
		fn(c)
	}
	ph := func(q string) string {
		var b strings.Builder
		n := 0
		for _, r := range q {
			if r == '?' {
				n++
				fmt.Fprintf(&b, "@p%d", n)
			} else {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return &libInstallRig{
		admin:   db,
		dialect: DialectMSSQL,
		name:    "sqlserver",
		schema:  "dbo",
		text:    func(col string) string { return "CAST(" + col + " AS nvarchar(max))" },
		store: func(tenant string) installableStore {
			return NewMSSQLStore(db).WithTenant(tenant)
		},
		exec: func(t *testing.T, tenant, q string, args ...any) {
			t.Helper()
			asTenant(t, tenant, func(c *sql.Conn) {
				if _, err := c.ExecContext(ctx, ph(q), args...); err != nil {
					t.Fatalf("exec %q: %v", q, err)
				}
			})
		},
		query: func(t *testing.T, tenant, q string, args ...any) []map[string]string {
			t.Helper()
			var out []map[string]string
			asTenant(t, tenant, func(c *sql.Conn) {
				rows, err := c.QueryContext(ctx, ph(q), args...)
				if err != nil {
					t.Fatalf("query %q: %v", q, err)
				}
				defer rows.Close()
				out = collectStringRows(t, rows)
			})
			return out
		},
		seedMember: func(t *testing.T, library string, m libMember) {
			t.Helper()
			validation := 0
			if m.validationDisabled() {
				validation = 1
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version, min_version,
					plugin_deps, entry_points, task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
				VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, @p14, @p15, @p16)`,
				library, m.name, m.version, m.wasm(), m.digest(), len(m.wasm()), m.abi(), m.minVersion(),
				m.pluginDeps(), fmt.Sprintf(`["alpha%d","beta%d"]`, m.variant, m.variant), m.taskQueue(), m.maxHistory(),
				m.dagSpec(), m.entrySchemas(), m.exposure, validation); err != nil {
				t.Fatalf("publish member %s: %v", m.name, err)
			}
		},
		seedStale: func(t *testing.T, tenant, library string, m libMember) {
			t.Helper()
			stale := staleOf(m)
			asTenant(t, tenant, func(c *sql.Conn) {
				if _, err := c.ExecContext(ctx, `
					INSERT INTO workflow_defs (tenant_id, name, version, library, abi_version, min_version, plugin_deps, entry_points,
						task_queue, max_history_length, dag_spec, entry_point_schemas, exposure, input_validation_disabled)
					VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, 0)`,
					tenant, m.name, m.version, library, stale.abi(), stale.minVersion(), stale.pluginDeps(),
					fmt.Sprintf(`["alpha%d","beta%d"]`, stale.variant, stale.variant), stale.taskQueue(), stale.maxHistory(),
					stale.dagSpec(), stale.entrySchemas(), stale.exposure); err != nil {
					t.Fatalf("seed a stale row for %s: %v", tenant, err)
				}
			})
		},
		registerPlugins: func(t *testing.T, names ...string) {
			t.Helper()
			if err := recordPluginsOn(ctx, db, DialectMSSQL, names); err != nil {
				t.Fatalf("register plugins %v: %v", names, err)
			}
		},
		forgetPlugin: func(t *testing.T, name string) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM admin.registered_plugins WHERE plugin_name = @p1`, name); err != nil {
				t.Fatalf("forget plugin %q: %v", name, err)
			}
		},
		cleanup: func(tenants []string, library string) {
			for _, tenant := range tenants {
				asTenant(t, tenant, func(c *sql.Conn) {
					_, _ = c.ExecContext(ctx, `DELETE FROM tenant_library_installs WHERE tenant_id = @p1`, tenant)
					_, _ = c.ExecContext(ctx, `DELETE FROM workflow_defs WHERE tenant_id = @p1`, tenant)
				})
			}
			_, _ = db.ExecContext(ctx, `DELETE FROM library.bundle_members WHERE library = @p1`, library)
			_, _ = db.ExecContext(ctx, `DELETE FROM library.bundles WHERE library = @p1`, library)
			_, _ = db.ExecContext(ctx, `DELETE FROM library.members WHERE library = @p1`, library)
		},
	}
}
