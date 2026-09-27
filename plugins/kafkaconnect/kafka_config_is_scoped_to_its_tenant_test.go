package kafkaconnect

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

// kafka_config carries a row-level policy, and the discovery loop names itself
// cross-tenant. cleat#1278.
//
// A SUITE DATABASE, because this test's migrations put a policy on a table and
// a policy is not scoped to the test that created it: it applies to every
// statement any package makes against that table.
//
// IT CONNECTS AS A ROLE THAT CANNOT BYPASS RLS. Every other plugin test in this
// repository connects as a superuser or the table owner, and PostgreSQL waves
// both straight past a policy -- so a tenant-isolation test written on
// testutil.TestDB's own connection cannot fail whatever the code does.
//
// POSTGRESQL FIXTURE: plugin.applyTenantScoping emits nothing on MySQL, and
// installs a policy on SQL Server too as of cleat#1552; this test covers the
// dialects, so running this elsewhere would assert that an absent policy does
// not apply.
func TestKafkaConfigIsScopedToItsTenant(t *testing.T) {
	su := testutil.SuiteTestDB(t, "kafkaconnect")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	p := &Plugin{dialect: dialect, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// ONE VALUE FOR "THE SCHEMA": plugin.RunMigrations defaults to "public" and
	// does not consult search_path. Deriving it a second way from
	// current_schema() is how the auditlog version of this test passed locally
	// and failed in the Tier 2 gate, which connects as a user whose
	// current_schema() is `cleat`.
	const schema = "public"
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("kafkaconnect migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.kafka_config TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	p.db = &engine.SQLDBAdapter{DB: rls, Dialect: dialect}

	// THE TWO CONNECTIONS MUST BE ON THE SAME DATABASE. If that is ever
	// violated, every write below succeeds, the rows land somewhere real, and
	// every read finds nothing -- which reads as a broken statement rather than
	// as two databases.
	var suDB, rlsDB string
	if err := su.QueryRowContext(ctx, `SELECT current_database()`).Scan(&suDB); err != nil {
		t.Fatalf("current_database on the superuser connection: %v", err)
	}
	if err := rls.QueryRowContext(ctx, `SELECT current_database()`).Scan(&rlsDB); err != nil {
		t.Fatalf("current_database on the RLS connection: %v", err)
	}
	if suDB != rlsDB {
		t.Fatalf("UNMEASURED: seeding on %q and reading on %q", suDB, rlsDB)
	}

	// START FROM AN EMPTY TABLE. SuiteTestDB persists between runs, so a second
	// invocation would otherwise count the first one's rows. The datadogexport
	// conversion passed on run 1 and failed on run 2 for exactly this.
	if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.kafka_config`); err != nil {
		t.Fatalf("clearing kafka_config before seeding: %v", err)
	}

	mine := uuid.MustParse("aaaa1111-0000-4000-8000-00000000cccc")
	theirs := uuid.MustParse("bbbb2222-0000-4000-8000-00000000dddd")

	// The OTHER tenant's row is seeded first. A table holding only `mine`
	// passes against a policy that is wrong, absent or bypassed, and a USING
	// clause over an empty table is never evaluated at all (cleat#1285).
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.kafka_config (tenant_id, id, name, brokers, topic, enabled)
		 VALUES ($1, gen_random_uuid(), 'theirs', 'b:9092', 't-theirs', true),
		        ($2, gen_random_uuid(), 'mine',   'b:9092', 't-mine',   true)`,
		theirs, mine); err != nil {
		t.Fatalf("seeding both tenants' configs: %v", err)
	}

	// 1. The policy filters.
	var seen int
	if err := p.db.QueryRow(plugin.ForTenant(ctx, mine),
		`SELECT count(*) FROM `+schema+`.kafka_config`).Scan(&seen); err != nil {
		t.Fatalf("counting kafka_config as one tenant: %v", err)
	}
	if seen != 1 {
		t.Errorf("a tenant-scoped read saw %d rows, want 1.\n\n"+
			"Two rows exist, one per tenant. Anything above 1 means the policy is not "+
			"filtering, and arm 2 below then proves nothing.", seen)
	}

	// 2. The discovery read is cross-tenant and must see both. Arm 1 is what
	//    makes this meaningful: "it saw everything" is also what no policy at
	//    all looks like.
	var discovered int
	if err := p.db.QueryRow(
		plugin.AcrossAllTenants(ctx, "test: the discovery query pollConfigs performs"),
		`SELECT count(*) FROM `+schema+`.kafka_config WHERE enabled = true`).Scan(&discovered); err != nil {
		t.Fatalf("the cross-tenant discovery read failed: %v", err)
	}
	if discovered != 2 {
		t.Errorf("the cross-tenant discovery read saw %d configs, want 2.\n\n"+
			"pollConfigs has to find which tenants have an enabled config, so it cannot be "+
			"scoped to one.", discovered)
	}
}

// pollConfigs must bind its bypass to a separate variable, never reassign ctx.
//
// Source-level because an inert narrowing has nothing observable at runtime: the
// read still returns the right rows, through a connection that is not scoped.
//
// The reach here is longer than in datadogexport, which is why this is worth its
// own guard rather than a comment: pollConfig is called INSIDE the cursor loop,
// so a reassigned ctx would carry the bypass through the REST proxy round trips
// and into eventtriggers.PublishEvent at the far end.
func TestPollConfigsDoesNotLeakItsBypassIntoThePoll(t *testing.T) {
	src, err := os.ReadFile("background.go")
	if err != nil {
		t.Fatalf("read background.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (p *Plugin) pollConfigs(")
	end := strings.Index(body, "func (p *Plugin) pollConfig(")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate pollConfigs and pollConfig: the scan is broken, so this " +
			"test is vacuous rather than passing")
	}
	// Strip line comments first: this file's own prose quotes the pattern, and
	// so does pollConfigs'.
	var lines []string
	for _, l := range strings.Split(body[start:end], "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	fn := strings.Join(lines, "\n")

	if regexp.MustCompile(`(?m)^\s*ctx\s*=\s*plugin\.AcrossAllTenants\(`).MatchString(fn) {
		t.Error("pollConfigs reassigns ctx with a cross-tenant bypass.\n\n" +
			"That bypass then covers pollConfig, every REST proxy round trip it makes, and " +
			"the eventtriggers publish at the end -- and any narrowing inside them is " +
			"ignored without a word. Bind it to a separate variable.")
	}
	// NON-VACUITY: with no bypass at all the check above passes for the wrong
	// reason and the discovery query is broken instead.
	if !strings.Contains(fn, "plugin.AcrossAllTenants(") {
		t.Error("pollConfigs names no cross-tenant bypass. Its discovery query reads " +
			"kafka_config across tenants and the policy refuses it without one.")
	}
	if !strings.Contains(fn, "p.pollConfig(ctx, c)") {
		t.Error("pollConfigs no longer passes the bare ctx to pollConfig; if it now passes " +
			"a derived one, check it is not the bypassed one.")
	}
}
