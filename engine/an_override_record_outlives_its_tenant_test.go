package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3600: the override record is an audit record of an operator-level act and is KEPT after
// the tenant it names is dropped, the same way a drop's own audit row is. Two pins, one for the
// cause and one for the effect, because only the first is checkable on a dialect whose drop
// procedure this repository cannot run.
//
// The cause: SQL Server's admin.drop_tenant deletes from EVERY table that has a column named
// tenant_id (migrations/mssql/003_procedures.sql), so a table carrying that name is not retained
// there. The column is for_tenant_id. This is not a style choice, and renaming it back would pass
// every other test on PostgreSQL while quietly making the record disposable on SQL Server.

type overrideRetentionDialect struct {
	name     string
	tdialect testutil.Dialect
	edialect Dialect
}

var overrideRetentionDialects = []overrideRetentionDialect{
	{"postgres", testutil.DialectPostgres, DialectPostgres},
	{"mysql", testutil.DialectMySQL, DialectMySQL},
	{"mssql", testutil.DialectMSSQL, DialectMSSQL},
}

func TestTheOverrideRecordHasNoColumnNamedTenantID(t *testing.T) {
	for _, tc := range overrideRetentionDialects {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, tc.tdialect)

			schema := `'admin'`
			if tc.edialect == DialectMySQL {
				schema = `DATABASE()`
			}
			has := func(column string) bool {
				var n int
				q := `SELECT count(*) FROM information_schema.columns WHERE table_schema = ` + schema +
					` AND table_name = 'plugin_dep_overrides' AND column_name = ` + tc.edialect.placeholder(1)
				if err := db.QueryRow(q, column).Scan(&n); err != nil {
					t.Fatalf("read the columns of plugin_dep_overrides: %v", err)
				}
				return n > 0
			}
			// The control: the query can see the table at all, or "no tenant_id" proves nothing.
			if !has("for_tenant_id") {
				t.Fatal("plugin_dep_overrides has no for_tenant_id column, so this query cannot see the table " +
					"and the absence checked next would read the same as a real one")
			}
			if has("tenant_id") {
				t.Error("plugin_dep_overrides has a column named tenant_id. SQL Server's admin.drop_tenant " +
					"deletes from every table with that column, so the record would not outlive its tenant there; " +
					"the column is for_tenant_id on purpose (see migrations/*/008_registered_plugins.sql)")
			}
		})
	}
}

// The effect, on the dialects that have a drop procedure: record an override for two tenants,
// drop one, and the record of BOTH is still there. MySQL has no admin.drop_tenant to run.
func TestTheOverrideRecordOutlivesItsTenant(t *testing.T) {
	for _, tc := range overrideRetentionDialects {
		if tc.edialect == DialectMySQL {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, tc.tdialect)
			ctx := context.Background()
			d := tc.edialect
			tbl := d.pluginDepOverridesTable()
			reg := &PluginNameRegistry{DB: db, Dialect: d}

			stamp := time.Now().UnixNano() % 1_000_000_000_000
			victim := fmt.Sprintf("0d3600a0-0000-4000-8000-%012d", stamp)
			bystander := fmt.Sprintf("0d3600b0-0000-4000-8000-%012d", stamp)
			def := fmt.Sprintf("override-retained-%d-%s", stamp, tc.name)

			insertTenant := `INSERT INTO admin.tenants (tenant_id, name) VALUES (` + d.placeholder(1) + `, ` + d.placeholder(2) + `)`
			if d == DialectPostgres {
				insertTenant += ` ON CONFLICT DO NOTHING`
			}
			for label, tn := range map[string]string{"victim": victim, "bystander": bystander} {
				if _, err := db.ExecContext(ctx, insertTenant, tn, fmt.Sprintf("override-retained-%s-%d", label, stamp)); err != nil {
					t.Fatalf("seed tenant %s: %v", tn, err)
				}
				if err := reg.RecordPluginDepOverride(ctx, PluginDepOverride{
					TenantID: tn, Definition: def, Version: 1, Via: "rest", Actor: "retention-test",
					Missing: []MissingPlugin{{Name: "never-registered", Constraint: ">=1", DeclaredBy: DeclaredByBinary}},
				}); err != nil {
					t.Fatalf("record an override for %s: %v", tn, err)
				}
			}
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM `+tbl+` WHERE definition_name = `+d.placeholder(1), def)
				_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+d.placeholder(1), bystander)
			})

			count := func(table, column, tenant string) int {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+column+` = `+d.placeholder(1), tenant).Scan(&n); err != nil {
					t.Fatalf("count %s for %s: %v", table, tenant, err)
				}
				return n
			}
			// Before anything is deleted, so a zero afterwards is not a row that was never there.
			for _, tn := range []string{victim, bystander} {
				if got := count(tbl, "for_tenant_id", tn); got != 1 {
					t.Fatalf("PRECONDITION FAILED: %d override row(s) for %s before the drop, want 1", got, tn)
				}
			}

			var dropSQL string
			switch d {
			case DialectPostgres:
				dropSQL = `SELECT admin.drop_tenant($1, 'public')`
			case DialectMSSQL:
				dropSQL = `EXEC admin.drop_tenant @tenant_id = @p1`
			}
			if _, err := db.ExecContext(ctx, dropSQL, victim); err != nil {
				t.Fatalf("admin.drop_tenant(victim): %v", err)
			}
			// Did the drop run at all? A procedure that did nothing leaves the record for the same reason.
			if got := count("admin.tenants", "tenant_id", victim); got != 0 {
				t.Fatalf("PRECONDITION FAILED: the victim's admin.tenants row survived the drop (%d), so the "+
					"procedure did not run to completion and the counts below say nothing", got)
			}

			for _, tn := range []struct{ id, label string }{{victim, "victim"}, {bystander, "bystander"}} {
				if got := count(tbl, "for_tenant_id", tn.id); got != 1 {
					t.Errorf("the %s has %d override row(s) after the drop, want 1: the record is kept after a "+
						"tenant is dropped", tn.label, got)
				}
			}
		})
	}
}
