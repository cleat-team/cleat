package ratelimiter

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// Both of this plugin's tables carry a policy, and both background writers name
// themselves cross-tenant. cleat#1278.
//
// TWO TABLES, scoped for different reasons: rate_limits is one tenant's
// configuration, rate_counter is one tenant's state. Neither is a
// global-by-design table like datadogexport's plugin_lease, which has no
// tenant_id at all.
//
// Connects as a role that CANNOT bypass RLS -- a superuser or table owner is
// waved past a policy, so a tenant-isolation test on testutil.TestDB's own
// connection cannot fail whatever the code does. PostgreSQL only, because
// applyTenantScoping emits nothing on MySQL, which has no row-level security.
// It DOES install a policy on SQL Server as of cleat#1552.
func TestRateLimitsAreScopedToTheirTenant(t *testing.T) {
	su := testutil.SuiteTestDB(t, "ratelimiter")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	p := &Plugin{dialect: dialect, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// One value for "the schema": RunMigrations defaults to "public" and does
	// not consult search_path. Asking current_schema() separately is how the
	// auditlog version of this failed in the Tier 2 gate.
	const schema = "public"
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("ratelimiter migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.rate_limits TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.rate_counter TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	p.db = &engine.SQLDBAdapter{DB: rls, Dialect: dialect}

	// Both connections must be on the same database, or every write lands
	// somewhere real and every read finds nothing -- which reads as a broken
	// statement rather than as two databases.
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

	// SuiteTestDB persists between runs; without this a second invocation
	// counts the first one's rows.
	for _, tbl := range []string{"rate_limits", "rate_counter"} {
		if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.`+tbl); err != nil {
			t.Fatalf("clearing %s before seeding: %v", tbl, err)
		}
	}

	mine := uuid.MustParse("cccc3333-0000-4000-8000-00000000eeee")
	theirs := uuid.MustParse("dddd4444-0000-4000-8000-00000000ffff")
	old := time.Now().UTC().Add(-2 * time.Hour)

	// The OTHER tenant's rows first. A table holding only `mine` passes against
	// a policy that is wrong, absent or bypassed, and a USING clause over an
	// empty table is never evaluated at all (cleat#1285).
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.rate_limits (tenant_id, limit_key, max_requests, window_seconds)
		 VALUES ($1,'k',10,60), ($2,'k',10,60)`, theirs, mine); err != nil {
		t.Fatalf("seeding rate_limits for both tenants: %v", err)
	}
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.rate_counter (tenant_id, limit_key, window_start, count)
		 VALUES ($1,'k',$3,1), ($2,'k',$3,1)`, theirs, mine, old); err != nil {
		t.Fatalf("seeding rate_counter for both tenants: %v", err)
	}

	// 1 and 2. BOTH tables filter. Checking only one would leave the other
	// unverified while looking thorough -- and they are listed together in one
	// TenantScoped, so a typo in either name is silent.
	for _, tbl := range []string{"rate_limits", "rate_counter"} {
		var seen int
		if err := p.db.QueryRow(plugin.ForTenant(ctx, mine),
			`SELECT count(*) FROM `+schema+`.`+tbl).Scan(&seen); err != nil {
			t.Fatalf("counting %s as one tenant: %v", tbl, err)
		}
		if seen != 1 {
			t.Errorf("a tenant-scoped read of %s saw %d rows, want 1.\n\n"+
				"Two rows exist, one per tenant. Anything above 1 means %s is not covered by "+
				"a policy -- check it is named in the TenantScoped list, spelled correctly.",
				tbl, seen, tbl)
		}
	}

	// 3. reload's read is cross-tenant: serving every tenant is the point.
	var allLimits int
	if err := p.db.QueryRow(
		plugin.AcrossAllTenants(ctx, "test: the read reload performs"),
		`SELECT count(*) FROM `+schema+`.rate_limits`).Scan(&allLimits); err != nil {
		t.Fatalf("the cross-tenant reload read failed: %v", err)
	}
	if allLimits != 2 {
		t.Errorf("the cross-tenant read saw %d limits, want 2 -- reload loads every tenant's "+
			"limits into one bucket map, so scoping it to one would silently rate-limit "+
			"everybody by somebody else's configuration", allLimits)
	}

	// 4. prune's sweep is cross-tenant and must reach BOTH tenants' counters.
	//    Arm 2 is what makes this meaningful: "it deleted everything" is also
	//    what no policy at all looks like.
	deleted, err := p.db.Exec(
		plugin.AcrossAllTenants(ctx, "test: the sweep pruneRateCounters performs"),
		plugin.Rebind(`DELETE FROM `+schema+`.rate_counter WHERE window_start < $1`, dialect),
		time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("the cross-tenant prune failed: %v\n\n"+
			"If this is \"cleat.tenant_id is not set\", pruneRateCounters is not naming "+
			"itself with plugin.AcrossAllTenants.", err)
	}
	if deleted != 2 {
		t.Errorf("the prune deleted %d counters, want 2 (one per tenant).\n\n"+
			"A sweep that reaches only its own tenant's expired counters leaves every other "+
			"tenant's to accumulate forever and reports success doing it.", deleted)
	}
}

// Both background writers must bind their bypass to a separate variable.
//
// Source-level because an inert narrowing has nothing observable at runtime.
// Neither function makes a downstream call with its context TODAY, so a
// reassignment would be harmless now -- and stops being harmless the first time
// someone adds one, silently. kafkaconnect's pollConfigs is the same shape with
// the reach already real.
func TestBackgroundWritersDoNotLeakTheirBypass(t *testing.T) {
	src, err := os.ReadFile("background.go")
	if err != nil {
		t.Fatalf("read background.go: %v", err)
	}
	// Strip line comments: this file and background.go both quote the pattern
	// in prose, and a search cannot tell a thing from a sentence about it.
	var lines []string
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	body := strings.Join(lines, "\n")

	if regexp.MustCompile(`(?m)^\s*ctx\s*=\s*plugin\.AcrossAllTenants\(`).MatchString(body) {
		t.Error("a background writer reassigns ctx with a cross-tenant bypass.\n\n" +
			"Bind it to a separate variable. Reassigning carries the bypass into anything " +
			"the function later calls, where a narrowing is ignored without a word -- " +
			"beginTenantTx tests CrossTenant before the tenant.")
	}
	// NON-VACUITY, per writer: with no bypass at all the check above passes for
	// the wrong reason and the statement is refused by the policy instead.
	for _, want := range []string{"loadCtx", "pruneCtx"} {
		if !strings.Contains(body, want) {
			t.Errorf("background.go names no %s. Both reload and pruneRateCounters read or "+
				"write across tenants and the policies refuse them without a named bypass.", want)
		}
	}
}
