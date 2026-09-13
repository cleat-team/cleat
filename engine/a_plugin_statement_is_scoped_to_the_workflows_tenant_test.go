package engine

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestAPluginStatementIsScopedToTheWorkflowsTenant is cleat#1278, and it is the
// half that costs a database.
//
// TestBothPluginCallPathsCarryTheTenant proves the value reaches the context a
// plugin function is handed. That is necessary and it is not the claim anyone
// cares about. The claim is that a plugin's SQL, issued over a host call,
// against a table carrying the policy plugin/migration.go:500 creates, now sees
// its own tenant's rows and no others. This runs the whole chain -- session
// tenant -> pluginCallContext -> plugin function -> SQLDBAdapter ->
// beginTenantTx -> set_config -> the policy -- and asserts the far end.
//
// IT CONNECTS AS A ROLE THAT CANNOT BYPASS RLS, AND THAT IS NOT A DETAIL.
// cleat#1278's own third finding is that every plugin test in this repository
// connects as a superuser or the table owner, and PostgreSQL lets both past a
// policy -- kvstore's suite passed identically against a table with no policy
// on it. A test of tenant isolation written on testutil.TestDB's connection
// cannot fail, whatever the code does. Hence OpenPostgresRLSTestDB.
//
// THE NEGATIVE CONTROL RUNS FIRST, for the same reason. Before believing that
// this tenant sees one row, the test seeds a row belonging to a DIFFERENT
// tenant and requires the policy to exclude it. Seeding only the row that
// should be visible would pass against a policy that is wrong, absent, or
// bypassed -- and a USING clause over an empty table is never evaluated at all
// (cleat#1285), so an unseeded table is the emptiest green of the three.
func TestAPluginStatementIsScopedToTheWorkflowsTenant(t *testing.T) {
	su := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	// Named per-process: engine tests share one development database with other
	// checkouts, so a fixed name collides with a concurrent run.
	table := fmt.Sprintf("plugin_tenant_probe_%d", os.Getpid())
	ctx := context.Background()
	t.Cleanup(func() { su.ExecContext(ctx, `DROP TABLE IF EXISTS `+table) })

	const (
		mine   = "3f2b1c00-0000-4000-8000-00000000abcd"
		theirs = "7c8d9e00-0000-4000-8000-00000000dcba"
	)
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ` + table,
		`CREATE TABLE ` + table + ` (tenant_id uuid NOT NULL, k text NOT NULL)`,
		`ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`,
		// Verbatim the policy plugin.applyTenantScoping emits.
		`CREATE POLICY ` + table + `_tenant_isolation ON ` + table +
			` FOR ALL USING (cleat.tenant_row_is_visible(tenant_id))`,
	} {
		if _, err := su.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture %.70q: %v", stmt, err)
		}
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	if _, err := su.ExecContext(ctx,
		`GRANT SELECT, INSERT ON `+table+` TO `+testutil.PostgresRLSTestRole); err != nil {
		t.Fatalf("granting the RLS role access to the fixture: %v", err)
	}

	// One row for each tenant. THEIRS is the one the policy has to exclude; a
	// table holding only MINE would pass against a broken policy.
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+table+` VALUES ($1,'mine'), ($2,'theirs')`, mine, theirs); err != nil {
		t.Fatalf("seeding both tenants: %v", err)
	}

	// The plugin: it does nothing but count what it can see, through the
	// adapter a real plugin is handed.
	adapter := &SQLDBAdapter{DB: rls, Dialect: plugin.DialectPostgres}
	var seen int
	var seenErr error
	pr := NewPluginRegistry()
	pr.RegisterWithPolicy("probe-plugin", "count",
		func(c context.Context, inputJSON string) (string, error) {
			seenErr = adapter.QueryRow(c, `SELECT count(*) FROM `+table).Scan(&seen)
			return `{}`, nil
		}, ReplayPolicy{})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.tenantID = mine

	buf := make([]byte, 256)
	if res := s.PluginCall(contextWithRawMemBuf(ctx, buf), nil,
		"probe-plugin", "count", `{}`, 0, 255); byte(res&0xFF) != 0 {
		t.Fatalf("the plugin call itself failed (errCode %d)", byte(res&0xFF))
	}

	if seenErr != nil {
		t.Fatalf("the plugin's statement failed: %v\n"+
			"  Before cleat#1278 this was exactly \"cleat.tenant_id is not set\": the\n"+
			"  host-call path set plugin.CallContext.TenantID and nothing put the value\n"+
			"  in tenantctx, which is the only carrier beginTenantTx reads.", seenErr)
	}
	if seen != 1 {
		t.Fatalf("the plugin saw %d rows, want 1.\n"+
			"  2 means the policy did not filter -- check that the connection is the\n"+
			"  RLS role and not a superuser, which bypasses policies unconditionally.\n"+
			"  0 means it filtered on the wrong tenant.", seen)
	}
}
