package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
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

	ctx := context.Background()

	// SCHEMA-QUALIFIED, AND THAT IS NOT TIDINESS. The engine schema is not
	// always `public`: a per-suite deployment migrates into its own schema and
	// reaches it through search_path. su carries that search_path; the RLS
	// connection is opened from a DSN and does not, so an UNQUALIFIED name
	// created here is invisible there.
	//
	// Measured: CI's `Test Go (engine)` passed this test and `Cluster
	// Integration Tests` failed it, same commit, on
	//
	//	pq: relation "plugin_tenant_probe_19591" does not exist (42P01)
	//
	// reproduced locally by migrating into a schema named sx and pointing
	// CLEAT_TEST_POSTGRES at it with search_path=sx. Qualifying makes the two
	// jobs ask the same question.
	var schema string
	if err := su.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("resolving the schema the engine tables actually live in: %v", err)
	}
	// Named per-process: engine tests share one development database with other
	// checkouts, so a fixed name collides with a concurrent run.
	table := fmt.Sprintf("%s.plugin_tenant_probe_%d", schema, os.Getpid())
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
		// The PRE-1490 shape, kept deliberately. applyTenantScoping now emits
		// two role-scoped policies instead of this CASE (cleat#1490), and the
		// new shape is covered by
		// a_tenant_policy_keeps_its_index_and_its_boundary_test.go. This
		// fixture stays on cleat.tenant_row_is_visible because policies
		// created before migration 074 keep it until their plugin's migrations
		// are re-applied -- which never happens for an already-recorded
		// version -- so the tenant bridge must go on working against it. The
		// POLICY name is a bare identifier -- it is scoped to the table, not to
		// a schema -- so only the table reference is qualified.
		`CREATE POLICY plugin_tenant_probe_policy ON ` + table +
			` FOR ALL USING (cleat.tenant_row_is_visible(tenant_id))`,
	} {
		if _, err := su.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture %.70q: %v", stmt, err)
		}
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	// SetupPostgresRLSRole grants USAGE on public and cleat only, by name. When
	// the engine schema is neither, the role cannot reach into it at all.
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT ON ` + table + ` TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access to the fixture: %v\n  %s", err, g)
		}
	}

	// PRECONDITION, CHECKED RATHER THAN ASSUMED. If the RLS connection cannot
	// resolve the fixture, everything below reports 42P01 from inside the
	// plugin -- which reads as a finding about the tenant bridge and is not one.
	// This is the UNMEASURED case: say so here, where the cause is visible.
	var visible bool
	if err := rls.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&visible); err != nil {
		t.Fatalf("asking the RLS connection whether it can see %s: %v", table, err)
	}
	if !visible {
		t.Fatalf("UNMEASURED: the RLS-role connection cannot resolve %s, so this test "+
			"cannot observe the policy at all. Nothing below would be about cleat#1278.", table)
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
		// DISCRIMINATE, because the first draft of this message did not. It
		// attributed every failure to the tenant bridge, and the failure it
		// actually met first was a schema-resolution error -- which read as a
		// finding about cleat#1278 and was not one.
		hint := "  This is NOT the cleat#1278 signature. Read the error above on its own terms;\n" +
			"  a 42P01 here means the fixture is not resolvable from the RLS connection."
		if strings.Contains(seenErr.Error(), "cleat.tenant_id is not set") {
			hint = "  This IS the cleat#1278 signature: the host-call path set\n" +
				"  plugin.CallContext.TenantID and nothing put the value in tenantctx,\n" +
				"  which is the only carrier beginTenantTx reads."
		}
		t.Fatalf("the plugin's statement failed: %v\n%s", seenErr, hint)
	}
	if seen != 1 {
		t.Fatalf("the plugin saw %d rows, want 1.\n"+
			"  2 means the policy did not filter -- check that the connection is the\n"+
			"  RLS role and not a superuser, which bypasses policies unconditionally.\n"+
			"  0 means it filtered on the wrong tenant.", seen)
	}
}
