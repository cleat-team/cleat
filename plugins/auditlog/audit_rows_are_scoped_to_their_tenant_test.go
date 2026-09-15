package auditlog

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// audit_events carries a row-level policy, and the two background writers were
// resolved in OPPOSITE directions. cleat#1278.
//
// This is the half that costs a database, and the half no conversion has yet
// committed: cleat#1511 converted jobqueue and verified it by hand. A
// conversion that is only verified by hand is verified once, by its author, on
// the day it lands.
//
// IT CONNECTS AS A ROLE THAT CANNOT BYPASS RLS, WHICH IS THE WHOLE TEST.
// cleat#1278's third finding is that every plugin test in this repository
// connects as a superuser or the table owner, and PostgreSQL lets both straight
// past a policy -- kvstore's suite passed identically against a table with no
// policy on it at all. A tenant-isolation test written on testutil.TestDB's own
// connection cannot fail, whatever the code does.
//
// POSTGRESQL ONLY, and not by omission: row-level security is the mechanism, and
// plugin.applyTenantScoping emits nothing on MySQL, and installs a policy on
// SQL Server too as of cleat#1552. Running
// this against MySQL or SQL Server would assert that an absent policy does not
// apply.
func TestAuditRowsAreScopedToTheirTenant(t *testing.T) {
	// A SUITE DATABASE, NOT THE SHARED ONE. This test's migrations put a
	// row-level policy on audit_events, and under testutil.TestDB that table
	// lives in the database every other package is using. A policy is not
	// scoped to the test that created it: it applies to every statement any
	// package makes against that table, and their writes and deletes apply to
	// this test's rows. The failures that produces name neither the sharing nor
	// the policy -- they point at whatever statement happened to run next.
	//
	// By construction this applies to every TenantScoped conversion test, since
	// enabling RLS on a shared table is what they all do.
	su := testutil.SuiteTestDB(t, "auditlog")
	t.Cleanup(func() { su.Close() })
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)

	ctx := context.Background()
	dialect := plugin.DialectPostgres
	p := &Plugin{
		dialect: dialect,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		config:  Config{RetentionDays: 1},
	}

	// ONE VALUE FOR "THE SCHEMA", PASSED IN RATHER THAN ASKED FOR TWICE.
	//
	// This test first derived it from `SELECT current_schema()` and granted on
	// that, which is how it failed in the Tier 2 gate with
	//
	//	pq: relation "cleat.audit_events" does not exist (42P01)
	//
	// plugin.RunMigrations defaults its schema to "public" and does NOT consult
	// search_path, so the table is created in public wherever it runs. The gate
	// connects as user `cleat`, where current_schema() is `cleat`. Two
	// derivations of "the schema", agreeing on every machine whose
	// current_schema() happens to be public, and disagreeing in CI.
	//
	// Passing it explicitly makes them the same value by construction rather
	// than by coincidence.
	const schema = "public"

	// The schema comes from the plugin's OWN migrations, including the v2 that
	// declares TenantScoped -- so the policy under test is the one the runtime
	// emits, not one written by hand here to fit the assertion.
	if err := plugin.RunMigrations(ctx, su, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}},
		plugin.WithSchema(schema)); err != nil {
		t.Fatalf("auditlog migrations: %v", err)
	}

	rls := testutil.OpenPostgresRLSTestDB(t, su)
	t.Cleanup(func() { rls.Close() })
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testutil.PostgresRLSTestRole,
		`GRANT SELECT, INSERT, DELETE ON ` + schema + `.audit_events TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := su.ExecContext(ctx, g); err != nil {
			t.Fatalf("granting the RLS role access: %v\n  %s", err, g)
		}
	}
	p.db = &engine.SQLDBAdapter{DB: rls, Dialect: dialect}

	mine := uuid.MustParse("3f2b1c00-0000-4000-8000-00000000abcd")
	theirs := uuid.MustParse("7c8d9e00-0000-4000-8000-00000000dcba")

	// PRECONDITION, CHECKED RATHER THAN ASSUMED. If the RLS connection cannot
	// resolve the table, every arm below fails with 42P01 from inside the
	// plugin, which reads as a finding about tenant scoping and is not one.
	var visible bool
	if err := rls.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, schema+".audit_events").Scan(&visible); err != nil {
		t.Fatalf("asking the RLS connection whether it can see audit_events: %v", err)
	}
	if !visible {
		t.Fatal("UNMEASURED: the RLS-role connection cannot resolve audit_events, so this " +
			"test cannot observe the policy at all")
	}

	// START FROM AN EMPTY TABLE, and this is not hygiene -- it is the
	// difference between arm 1 measuring anything and measuring nothing.
	//
	// testutil.TestDB hands back a database that persists between runs, so
	// audit_events still holds `mine`'s row from the previous invocation.
	// Arm 1 counts rows for `mine` and would find that one, report 1, and pass
	// -- against a recordAudit with its tenant deliberately removed. Found by
	// falsifying: mutation 2 was expected to stop the test at arm 1's Fatalf
	// and instead fell through to arm 3, which is only possible if arm 1 saw a
	// row it did not write.
	if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.audit_events`); err != nil {
		t.Fatalf("clearing audit_events before seeding: %v", err)
	}

	// THE NEGATIVE CONTROL IS SEEDED FIRST, and it is the other tenant's row.
	// A table holding only `mine` passes against a policy that is wrong,
	// absent, or bypassed -- and a USING clause over an EMPTY table is never
	// evaluated at all (cleat#1285), which is the emptiest green of the three.
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.audit_events (id, tenant_id, method, path, status_code, duration_ms, timestamp)
		 VALUES (gen_random_uuid(), $1, 'GET', '/theirs', 200, 1, now())`, theirs); err != nil {
		t.Fatalf("seeding the other tenant's row: %v", err)
	}

	// ARM 1 -- THE RESTORED TENANT. recordAudit builds its insert context from
	// context.Background() so an audit row survives a cancelled request, and
	// used to drop the tenant doing so. Against the policy that is not a
	// filtered write, it is a refusal:
	//
	//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
	//
	// It is NOT marked AcrossAllTenants, deliberately: it writes one tenant's
	// row and had the tenant in hand all along. Bypassing here would pass this
	// arm and silently disable isolation for every audit write.
	p.recordAudit(ctx, mine, "GET", "/mine", 200, "127.0.0.1", "probe", time.Millisecond)

	var mineCount int
	if err := su.QueryRowContext(ctx,
		`SELECT count(*) FROM `+schema+`.audit_events WHERE tenant_id = $1`, mine).Scan(&mineCount); err != nil {
		t.Fatalf("counting the row recordAudit should have written: %v", err)
	}
	if mineCount != 1 {
		t.Fatalf("recordAudit wrote %d rows for its tenant, want 1.\n\n"+
			"It derives its insert context from context.Background() so the write survives "+
			"a cancelled request, which discards the tenant the request context carried. "+
			"Against audit_events' policy an insert with no tenant is refused outright -- "+
			"and recordAudit logs its error and returns, so the only symptom is an empty "+
			"table, which is indistinguishable from an audit log for a quiet system.",
			mineCount)
	}

	// ARM 2 -- THE POLICY ACTUALLY APPLIES. This is the control for arm 3:
	// "the sweep deleted everything" is also what a policy that never applies
	// looks like, so something has to establish that the policy DOES filter.
	// Two rows exist; a read scoped to `mine` must see exactly one.
	var seen int
	scoped := auth.WithTenantID(ctx, mine)
	if err := p.db.QueryRow(scoped,
		`SELECT count(*) FROM `+schema+`.audit_events`).Scan(&seen); err != nil {
		t.Fatalf("counting through the plugin adapter as one tenant: %v", err)
	}
	//
	// FOUR FAILURE SIGNATURES, and they are not interchangeable -- each was
	// produced deliberately while falsifying this test:
	//
	//	policy permissive (USING (true))   this arm reports 2 rows
	//	policy DROPPED entirely            arm 1 fails instead: FORCE ROW LEVEL
	//	                                   SECURITY with no policy is
	//	                                   default-DENY, so the table becomes
	//	                                   wholly unreadable and the insert
	//	                                   never lands
	//	sweep not marked AcrossAllTenants  arm 3 fails: "cleat.tenant_id is not set"
	//	recordAudit without its tenant     arm 1 fails: "wrote 0 rows"
	//
	// The second is worth knowing because "drop the policy" is the obvious way
	// to check this arm and it does not test this arm at all.
	if seen != 1 {
		t.Errorf("a tenant-scoped read saw %d rows, want 1.\n\n"+
			"Two rows exist, one per tenant. Seeing 2 means the policy is not filtering -- "+
			"either it was never created, or this connection can bypass it, in which case "+
			"arm 3 below proves nothing either.", seen)
	}

	// ARM 3 -- THE NAMED BYPASS. Retention is global: the cutoff is a
	// timestamp and no tenant owns it. cleanupRetention marks its context with
	// plugin.AcrossAllTenants, so the policy lifts and the sweep reaches both
	// tenants' rows. Without the marking this errors rather than under-deleting,
	// which is the failure mode worth having -- but arm 2 is what makes the
	// success meaningful.
	if _, err := su.ExecContext(ctx,
		`UPDATE `+schema+`.audit_events SET timestamp = now() - interval '30 days'`); err != nil {
		t.Fatalf("ageing both rows past the retention cutoff: %v", err)
	}
	deleted, err := p.cleanupRetention(ctx)
	if err != nil {
		t.Fatalf("the retention sweep failed: %v\n\n"+
			"If this is \"cleat.tenant_id is not set\", the sweep is not naming itself with "+
			"plugin.AcrossAllTenants. Adding the policy and leaving the sweep bare are the "+
			"same change, which is why they are in one commit.", err)
	}
	// ARM 4 -- A BYPASS ALREADY IN SCOPE WINS, SILENTLY. plugin.ForTenant's doc
	// comment asserts this, and an ordering asserted only in prose is exactly
	// what rots: beginTenantTx tests CrossTenant BEFORE the tenant, so a
	// ForTenant inside an AcrossAllTenants scope is ignored without a word.
	//
	// That ordering is deliberate -- an admin endpoint rebuilding an index for
	// everyone runs on a request context, and resolving it the other way would
	// quietly scope the sweep to whoever called it. The hazard is that it is
	// silent, so it is pinned rather than described.
	//
	// NOT A DUPLICATE OF cleat#1515, AND THE DIFFERENCE IS WHICH LAYER HOLDS IT
	// UP. The precedence is enforced TWICE: beginTenantTx decides which GUCs it
	// sets, and cleat.tenant_row_is_visible's own CASE reads cleat.cross_tenant
	// before cleat.tenant_id (migrations/postgres/063). #1515 pins the Go half
	// by reading the settings directly -- which is the sharper test of that
	// half, since it can require cleat.tenant_id to be EMPTY rather than merely
	// overridden. This arm pins the COMPOSITE, through a real policy on a real
	// table: if a later migration flipped the CASE, #1515 would still pass and
	// this would fail. Two derivations of one rule, at different layers, kept
	// deliberately.
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.audit_events (id, tenant_id, method, path, status_code, duration_ms, timestamp)
		 VALUES (gen_random_uuid(), $1, 'GET', '/a', 200, 1, now()),
		        (gen_random_uuid(), $2, 'GET', '/b', 200, 1, now())`, mine, theirs); err != nil {
		t.Fatalf("re-seeding both tenants for the precedence arm: %v", err)
	}
	both := plugin.ForTenant(
		plugin.AcrossAllTenants(ctx, "precedence probe: the bypass must win"), mine)
	var underBypass int
	if err := p.db.QueryRow(both,
		`SELECT count(*) FROM `+schema+`.audit_events`).Scan(&underBypass); err != nil {
		t.Fatalf("counting under a bypassed-then-scoped context: %v", err)
	}
	if underBypass != 2 {
		t.Errorf("a ForTenant inside an AcrossAllTenants scope saw %d rows, want 2.\n\n"+
			"The bypass is tested first and must win, so the narrowing is ignored. If this "+
			"is 1, the precedence has been reversed -- and an admin sweep running on a "+
			"request context would silently narrow to whoever called it, which is the "+
			"class of answer this mechanism exists to make impossible.", underBypass)
	}
	if _, err := su.ExecContext(ctx, `DELETE FROM `+schema+`.audit_events`); err != nil {
		t.Fatalf("clearing after the precedence arm: %v", err)
	}
	if _, err := su.ExecContext(ctx,
		`INSERT INTO `+schema+`.audit_events (id, tenant_id, method, path, status_code, duration_ms, timestamp)
		 VALUES (gen_random_uuid(), $1, 'GET', '/mine', 200, 1, now() - interval '30 days'),
		        (gen_random_uuid(), $2, 'GET', '/theirs', 200, 1, now() - interval '30 days')`,
		mine, theirs); err != nil {
		t.Fatalf("re-seeding aged rows for the sweep arm: %v", err)
	}

	if deleted != 2 {
		t.Errorf("the retention sweep deleted %d rows, want 2 (one per tenant).\n\n"+
			"A sweep that deletes only its own tenant's expired rows leaves every other "+
			"tenant's to accumulate forever, and reports success while doing it.", deleted)
	}
}
