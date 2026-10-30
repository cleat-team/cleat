package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3600: each use of the plugin-dependency override is a durable row, written through the
// real recorder on all three dialects, and the application role can append to the table and
// cannot change or remove a row.
func TestAnOverrideIsRecordedInADurableRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect Dialect
	}{
		{"postgres", testutil.DialectPostgres, DialectPostgres},
		{"mysql", testutil.DialectMySQL, DialectMySQL},
		{"mssql", testutil.DialectMSSQL, DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, tc.tdialect)
			ctx := context.Background()
			reg := &PluginNameRegistry{DB: db, Dialect: tc.edialect}
			tbl := tc.edialect.pluginDepOverridesTable()

			def := fmt.Sprintf("override-def-%d-%s", time.Now().UnixNano(), tc.name)
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM `+tbl+` WHERE definition_name = `+tc.edialect.placeholder(1), def)
			})
			count := func() int {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM `+tbl+` WHERE definition_name = `+tc.edialect.placeholder(1), def).Scan(&n); err != nil {
					t.Fatalf("count the override rows: %v", err)
				}
				return n
			}
			if count() != 0 {
				t.Fatal("rows exist before anything was recorded, so the assertions below would prove nothing")
			}

			// Nothing missing is nothing to record, and is an error rather than an empty row.
			if err := reg.RecordPluginDepOverride(ctx, PluginDepOverride{TenantID: "t", Definition: def, Version: 1, Via: "rest"}); err == nil {
				t.Error("an override with nothing missing recorded without error")
			}
			if count() != 0 {
				t.Fatalf("%d rows written for an override with nothing missing", count())
			}

			err := reg.RecordPluginDepOverride(ctx, PluginDepOverride{
				TenantID: "tenant-uuid-1", Definition: def, Version: 4, Via: "cleatctl", Actor: "operator-x",
				Missing: []MissingPlugin{
					{Name: "plugin-one", Constraint: ">=1", DeclaredBy: DeclaredByBinary},
					{Name: "plugin-two", Constraint: "", DeclaredBy: DeclaredByRow},
				},
			})
			if err != nil {
				t.Fatalf("RecordPluginDepOverride: %v", err)
			}
			var tenant, via, actor, missing string
			var version int
			var at time.Time
			if err := db.QueryRow(`SELECT tenant_id, definition_version, via, actor, missing_plugins, occurred_at FROM `+tbl+` WHERE definition_name = `+tc.edialect.placeholder(1), def).
				Scan(&tenant, &version, &via, &actor, &missing, &at); err != nil {
				t.Fatalf("read the row back: %v", err)
			}
			if tenant != "tenant-uuid-1" || version != 4 || via != "cleatctl" || actor != "operator-x" {
				t.Errorf("row = tenant %q version %d via %q actor %q, lost something it was given", tenant, version, via, actor)
			}
			if time.Since(at) > time.Minute || time.Until(at) > time.Minute {
				t.Errorf("occurred_at = %v, not stamped now", at)
			}
			var got []overrideMissingJSON
			if err := json.Unmarshal([]byte(missing), &got); err != nil || len(got) != 2 ||
				got[0].Name != "plugin-one" || got[0].DeclaredBy != DeclaredByBinary || got[1].Name != "plugin-two" || got[1].DeclaredBy != DeclaredByRow {
				t.Errorf("missing_plugins = %q (err %v), want both names with who declared them", missing, err)
			}
		})
	}
}

// Append-only is a privilege on PostgreSQL and SQL Server: read from the catalog, not through the
// harness's stand-in role, which is granted by the harness and would only prove what it says.
func TestTheApplicationRoleCanOnlyAppendToTheOverrideRecord(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false} {
			var has bool
			if err := db.QueryRow(`SELECT has_table_privilege('cleat_app', 'admin.plugin_dep_overrides', $1)`, priv).Scan(&has); err != nil {
				t.Fatalf("has_table_privilege %s: %v", priv, err)
			}
			if has != want {
				t.Errorf("cleat_app %s on admin.plugin_dep_overrides = %v, want %v", priv, has, want)
			}
		}
	})
	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
		for _, perm := range []string{"UPDATE", "DELETE"} {
			var state string
			err := db.QueryRow(`
				SELECT state_desc FROM sys.database_permissions
				WHERE class = 1 AND major_id = OBJECT_ID(N'admin.plugin_dep_overrides')
				  AND grantee_principal_id = DATABASE_PRINCIPAL_ID(N'cleat_app_role') AND permission_name = @p1`, perm).Scan(&state)
			if err != nil || state != "DENY" {
				t.Errorf("cleat_app_role %s on admin.plugin_dep_overrides: state %q err %v, want DENY", perm, state, err)
			}
		}
	})
}
