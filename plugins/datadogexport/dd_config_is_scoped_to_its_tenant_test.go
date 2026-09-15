package datadogexport

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// dd_config carries a row-level policy; plugin_lease deliberately does not.
// cleat#1278.
//
// This plugin adds the judgement auditlog's conversion did not need: a plugin
// table is one of THREE things, and two of them look identical in the schema.
//
//	dd_config      tenant-scoped     -> TenantScoped, the policy applies
//	plugin_lease   global by design  -> no TenantScoped, no policy, and its
//	                                    loops need no bypass because nothing
//	                                    scopes them
//	(anything else) unscoped by oversight -> what cleat#1278 exists to fix
//
// plugin_lease has no tenant_id at all: one row for the whole fleet, and every
// worker of every tenant is meant to contend for it. Inventing a tenant column
// would elect one leader per tenant, which is not what leader election means
// here.
//
// A SUITE DATABASE, because this test's migrations put a policy on a table. A
// policy is not scoped to the test that created it -- it applies to every
// statement any package makes against that table, and their writes apply to
// these rows.
//
// POSTGRESQL ONLY: row-level security is the mechanism and
// plugin.applyTenantScoping emits nothing on the other two dialects.
func TestDDConfigIsScopedToItsTenant(t *testing.T) {
	su := testutil.SuiteTestDB(t, "datadogexport")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	p := &Plugin{
		dialect: dialect,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// ONE VALUE FOR "THE SCHEMA". plugin.RunMigrations defaults to "public" and
	// does not consult search_path; deriving it a second way from
	// current_schema() is how the auditlog version of this test passed locally
	// and failed in the Tier 2 gate, which connects as a user whose
	// current_schema() is `cleat`.
	const schema = "public"
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("datadogexport migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.dd_config TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.plugin_lease TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	p.db = &engine.SQLDBAdapter{DB: rls, Dialect: dialect}

	// THE TWO CONNECTIONS MUST BE ON THE SAME DATABASE. Nothing states this
	// invariant, and if it is ever violated every write below succeeds, the rows
	// land somewhere real, and every read finds nothing -- which reads as a
	// broken statement rather than as two databases. Adopted from the
	// cleat-ports session, who lost time to exactly that symptom.
	var suDB, rlsDB string
	if err := su.QueryRowContext(ctx, `SELECT current_database()`).Scan(&suDB); err != nil {
		t.Fatalf("current_database on the superuser connection: %v", err)
	}
	if err := rls.QueryRowContext(ctx, `SELECT current_database()`).Scan(&rlsDB); err != nil {
		t.Fatalf("current_database on the RLS connection: %v", err)
	}
	if suDB != rlsDB {
		t.Fatalf("UNMEASURED: the seeding connection is on %q and the reading connection on "+
			"%q. Every assertion below would compare a write in one database against a read "+
			"in another.", suDB, rlsDB)
	}

	mine := uuid.MustParse("11112222-0000-4000-8000-00000000aaaa")
	theirs := uuid.MustParse("33334444-0000-4000-8000-00000000bbbb")

	// START FROM EMPTY TABLES. testutil.SuiteTestDB hands back a database that
	// PERSISTS between runs, so a second invocation sees the first one's rows:
	// dd_config accumulates two per run and plugin_lease's primary key
	// collides. Measured, not predicted -- this test passed on its first run and
	// failed on its second with "saw 3 rows, want 1" and a duplicate key.
	//
	// This is the same defect the auditlog conversion hit one PR earlier. I
	// found it there, wrote it into that commit message, and reproduced it in
	// the next test I wrote -- which is the argument for it being a line in
	// every such test rather than a thing to remember.
	for _, tbl := range []string{"dd_config", "plugin_lease"} {
		if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.`+tbl); err != nil {
			t.Fatalf("clearing %s before seeding: %v", tbl, err)
		}
	}

	// The other tenant's row is seeded FIRST. A table holding only `mine`
	// passes against a policy that is wrong, absent, or bypassed, and a USING
	// clause over an empty table is never evaluated at all (cleat#1285).
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.dd_config (id, tenant_id, api_key, site, metrics_prefix, enabled)
		 VALUES (gen_random_uuid(), $1, 'k-theirs', 'datadoghq.com', 'cleat', true),
		        (gen_random_uuid(), $2, 'k-mine',   'datadoghq.com', 'cleat', true)`,
		theirs, mine); err != nil {
		t.Fatalf("seeding both tenants' configs: %v", err)
	}

	// 1. dd_config IS scoped: a read for one tenant sees one row of two.
	var seen int
	if err := p.db.QueryRow(plugin.ForTenant(ctx, mine),
		`SELECT count(*) FROM `+schema+`.dd_config`).Scan(&seen); err != nil {
		t.Fatalf("counting dd_config as one tenant: %v", err)
	}
	if seen != 1 {
		t.Errorf("a tenant-scoped read of dd_config saw %d rows, want 1.\n\n"+
			"Two rows exist, one per tenant. Anything above 1 means the policy is not "+
			"filtering, and arm 3 below then proves nothing.", seen)
	}

	// 2. plugin_lease is NOT scoped, and a write with no tenant in context must
	//    succeed. It has no tenant_id; every worker of every tenant contends for
	//    one row, and electing one leader per tenant is not leader election.
	//
	//    WHAT THIS DOES *NOT* GUARD, stated because my first version of this
	//    comment claimed it did. I falsified it by adding plugin_lease to
	//    TenantScoped, expecting this arm to report a policy refusal. It never
	//    ran: plugin.applyTenantScoping fails at MIGRATION time with
	//
	//      CREATE POLICY plugin_lease_tenant_isolation ON plugin_lease ...
	//      pq: column "tenant_id" does not exist (42703)
	//
	//    which is a better outcome than a test catching it, and means the
	//    "global by design" category is protected by construction rather than by
	//    vigilance. A table with no tenant_id cannot be given a policy at all.
	//
	//    So this arm guards the live behaviour -- an unscoped plugin table stays
	//    writable with no tenant in context -- and the misconfiguration it was
	//    written for is caught earlier and more loudly than it could catch it.
	if _, err := p.db.Exec(ctx,
		`INSERT INTO `+schema+`.plugin_lease (name, holder, expires_at)
		 VALUES ('datadog-export', 'worker-1', now() + interval '1 hour')`); err != nil {
		t.Errorf("writing the leader lease with no tenant in context failed: %v\n\n"+
			"plugin_lease is global by design -- no tenant_id, one row for the fleet. If "+
			"this is a policy refusal, it has been made TenantScoped, which would elect one "+
			"leader per tenant rather than one leader.", err)
	}

	// 3. The discovery query IS cross-tenant, and must see both.
	var discovered int
	if err := p.db.QueryRow(
		plugin.AcrossAllTenants(ctx, "test: the discovery query exportMetrics performs"),
		`SELECT count(*) FROM `+schema+`.dd_config WHERE enabled = true`).Scan(&discovered); err != nil {
		t.Fatalf("the cross-tenant discovery read failed: %v", err)
	}
	if discovered != 2 {
		t.Errorf("the cross-tenant discovery read saw %d configs, want 2.\n\n"+
			"exportMetrics has to find which tenants have an export configured, so it "+
			"cannot be scoped to one -- there is no tenant to scope it to until it "+
			"returns.", discovered)
	}

	// 4. THE NARROWING IS NOT INERT, which is the assertion this plugin exists
	//    to make and the one a careless refactor silently breaks.
	//
	//    exportForConfig narrows to cfg.TenantID with plugin.ForTenant. That
	//    only works because exportMetrics scopes its bypass to a SEPARATE
	//    variable rather than reassigning ctx: beginTenantTx tests CrossTenant
	//    first, so a bypass still in scope wins and the narrowing is ignored
	//    WITHOUT A WORD. The first draft of exportMetrics did reassign, and
	//    exportForConfig carried a comment claiming it received the unmarked
	//    parent. It did not.
	//
	//    Here the ordering is demonstrated directly: narrowing UNDER a bypass
	//    still sees both rows.
	var underBypass int
	if err := p.db.QueryRow(
		plugin.ForTenant(plugin.AcrossAllTenants(ctx, "test: bypass still in scope"), mine),
		`SELECT count(*) FROM `+schema+`.dd_config`).Scan(&underBypass); err != nil {
		t.Fatalf("counting under a bypassed-then-narrowed context: %v", err)
	}
	if underBypass != 2 {
		t.Errorf("narrowing under a bypass saw %d rows, want 2 -- the bypass must win.\n\n"+
			"If this is 1, the precedence has reversed, and exportMetrics' careful use of a "+
			"separate variable is no longer what makes exportForConfig's narrowing work.",
			underBypass)
	}
}

// exportMetrics must scope its bypass to a SEPARATE variable, never reassign ctx.
//
// TestDDConfigIsScopedToItsTenant pins the precedence -- a bypass in scope beats
// a narrowing -- but nothing there pins that exportMetrics actually keeps its
// bypass out of the context it hands onward. Those are different claims, and the
// second is the one a refactor breaks: `ctx = plugin.AcrossAllTenants(...)` is
// the natural way to write it, it compiles, every runtime test still passes, and
// exportForConfig's plugin.ForTenant becomes silently inert.
//
// That is not hypothetical. The first draft of exportMetrics reassigned ctx, and
// exportForConfig carried a comment claiming it received the unmarked parent.
// It did not.
//
// Source-level because there is nothing to observe at runtime: an inert
// narrowing produces a correct-looking read of one tenant's data through a
// connection that is not scoped to it, and the SQL predicate still filters.
func TestExportMetricsDoesNotLeakItsBypassToTheCaller(t *testing.T) {
	src, err := os.ReadFile("background.go")
	if err != nil {
		t.Fatalf("read background.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *Plugin) exportMetrics(")
	end := strings.Index(body, "func (p *Plugin) exportForConfig(")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate exportMetrics and exportForConfig: the scan is broken, " +
			"so this test is vacuous rather than passing")
	}
	fn := stripLineComments(body[start:end])

	if regexp.MustCompile(`(?m)^\s*ctx\s*=\s*plugin\.AcrossAllTenants\(`).MatchString(fn) {
		t.Error("exportMetrics reassigns ctx with a cross-tenant bypass.\n\n" +
			"The bypass then flows into exportForConfig, where plugin.ForTenant is ignored " +
			"without a word -- beginTenantTx tests CrossTenant before the tenant. Bind it to " +
			"a separate variable (discoverCtx) so the bypass covers the discovery query and " +
			"nothing else.")
	}
	// NON-VACUITY: if the bypass is gone entirely, the check above passes for
	// the wrong reason and the discovery query is broken instead.
	if !strings.Contains(fn, "plugin.AcrossAllTenants(") {
		t.Error("exportMetrics names no cross-tenant bypass at all. Its discovery query reads " +
			"dd_config across tenants and is refused by the policy without one.")
	}
	// WHAT THIS ACTUALLY PROTECTS is that exportForConfig does not receive the
	// BYPASSED context -- not that the argument is spelled "ctx".
	//
	// It used to require the literal `exportForConfig(ctx, cfg)`, which failed
	// the moment cleat#1611 passed a derived context carrying an originated
	// trace-id (`ectx := plugin.WithNewTrace(ctx)`). That was the guard asking
	// its own question correctly -- its message said "if it now passes a derived
	// one, check that it is not the bypassed one" -- and the answer was that it
	// is derived from the clean ctx. Checking the property rather than the
	// spelling means the next legitimate derivation does not have to re-litigate
	// it, while an ILLEGITIMATE one still fails.
	m := regexp.MustCompile(`exportForConfig\((\w+), cfg\)`).FindStringSubmatch(fn)
	if m == nil {
		t.Error("exportMetrics does not call exportForConfig(<ctx>, cfg) in a form this guard " +
			"can read. Keep the call shape simple enough to check, or teach this guard the new " +
			"one -- an unreadable call is an unchecked one.")
	} else {
		arg := m[1]
		if arg == "discoverCtx" {
			t.Error("exportForConfig is called with discoverCtx, the CROSS-TENANT context.\n\n" +
				"plugin.ForTenant inside it is then ignored without a word -- beginTenantTx " +
				"tests CrossTenant before the tenant -- so one tenant's export would read every " +
				"tenant's rows.")
		}
		// A derived context is fine unless it derives FROM the bypass.
		derived := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(arg) + `\s*:?=\s*.*discoverCtx`)
		if derived.MatchString(fn) {
			t.Errorf("exportForConfig is called with %q, which is derived from discoverCtx and "+
				"therefore still carries the cross-tenant bypass.", arg)
		}
	}
}

// stripLineComments removes // comments so prose ABOUT a pattern is not read as
// the pattern. This file's own comments quote `ctx = plugin.AcrossAllTenants`.
func stripLineComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
