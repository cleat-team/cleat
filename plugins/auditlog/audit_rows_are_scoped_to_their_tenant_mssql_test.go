package auditlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestAuditRowsAreScopedToTheirTenantOnMSSQL is cleat#2714: the SQL Server
// counterpart to TestAuditRowsAreScopedToTheirTenant, which is PostgreSQL-only
// by its own doc comment. plugins/auditlog/migrations.go's v2 declares
// TenantScoped: []string{"audit_events"}, and plugin/migration.go's
// applyTenantScoping dispatches that to applyTenantScopingMSSQL, which
// installs a real CREATE SECURITY POLICY on audit_events -- cleat#1552's claim
// verified directly here, not assumed. Nothing exercised it on this dialect
// before this file: `grep -rln "audit_events" --include="*mssql*test*.go" .`
// returned nothing.
//
// UNLIKE THE POSTGRES ARM, THIS NEEDS NO SPECIAL NON-SUPERUSER ROLE.
// PostgreSQL's row-level security is bypassed by a superuser or the table
// owner -- which is why the sibling test builds testutil.OpenPostgresRLSTestDB
// and GRANTs a dedicated low-privilege role just to make the policy bind at
// all. SQL Server's SECURITY POLICY has no such escape hatch: it applies to
// every principal, sa included (the same asymmetry documented on
// cleat#2378/#2825's oauthprovider test, measured there directly). So
// testutil.MSSQLTestDB's ordinary connection is already the right connection
// to test through, and it is the one plugin.RunMigrations installs the
// policy against.
//
// THIS DATABASE IS SHARED, AND THAT IS AN EXISTING PRECEDENT, NOT A NEW RISK.
// There is no MSSQL equivalent of testutil.SuiteTestDB, so unlike the Postgres
// arm this cannot isolate audit_events in a database of its own. That is not
// new exposure: engine/plugin_migrations_test.go's TestPluginMigrations_
// AllDialects already runs every linked plugin's migrations -- auditlog
// included -- against this same testutil.MSSQLTestDB, so the policy is
// already installed on the shared database regardless of this file, and every
// other MSSQL-dialect plugin behavioral test (oauthprovider's, ratelimiter's,
// kafkaconnect's, datadogexport's siblings) already shares it the same way.
// What this test owns is not truncating the table, cleaning up only its own
// two tenants' rows, and using tenant ids no other test writes under.
func TestAuditRowsAreScopedToTheirTenantOnMSSQL(t *testing.T) {
	// cleat#2249: this loop's whole body lives behind the Dialect == MSSQL
	// guard below, and testutil.NewPluginTestBackends only ever includes an
	// MSSQL backend when CLEAT_TEST_MSSQL is set -- PostgreSQL is the only
	// one it attempts unconditionally. So when CLEAT_TEST_MSSQL is unset,
	// the returned slice has no MSSQL entry, every iteration's `continue`
	// fires, and the function returns having asserted nothing -- a genuine
	// PASS in ~0.02s, with no t.Skip anywhere to make that visible. Measured
	// directly: with CLEAT_TEST_POSTGRES set and CLEAT_TEST_MSSQL unset,
	// `go test -run TestAuditRowsAreScopedToTheirTenantOnMSSQL -v` printed
	// `--- PASS (0.02s)` and nothing else. This explicit skip gives the
	// skip-budget/skip-ledger guards -- which only see t.Skip -- something
	// to see.
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMSSQL {
			continue
		}
		be := be
		defer be.Cleanup()

		ctx := context.Background()
		dialect := plugin.Dialect(string(be.Dialect))
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

		testutil.SetupFullSchema(t, be.DB, be.Dialect)

		seedPlugin := &Plugin{dialect: dialect, logger: quiet, config: Config{RetentionDays: 1}}
		if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
			[]*plugin.LoadedPlugin{{Plugin: seedPlugin, Healthy: true}}); err != nil {
			t.Fatalf("auditlog migrations on %s: %v", be.Name, err)
		}

		tenantA := uuid.New()
		tenantB := uuid.New()

		// A cross-tenant fixture connection, exactly as cleat#2378/#2825's
		// oauthprovider test uses: seeding a second tenant, and seeding/
		// clearing rows directly, both reach a plugin table the policy would
		// otherwise filter or block for an ordinary connection.
		fixtureDB := be.CrossTenantConn(t, ctx,
			"audit cross-tenant fixture cleat#2714: seeds a second tenant and rows directly")

		for _, seed := range []struct {
			id   uuid.UUID
			name string
		}{
			{tenantA, "cleat-2714-a-" + tenantA.String()[:8]},
			{tenantB, "cleat-2714-b-" + tenantB.String()[:8]},
		} {
			if _, err := fixtureDB.ExecContext(ctx,
				`INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
				seed.id.String(), seed.name); err != nil {
				t.Fatalf("seed tenant %s: %v", seed.id, err)
			}
		}
		defer func() {
			bg := context.Background()
			for _, tid := range []uuid.UUID{tenantA, tenantB} {
				if _, err := fixtureDB.ExecContext(bg,
					`DELETE FROM audit_events WHERE tenant_id = @p1`, tid.String()); err != nil {
					t.Errorf("cleanup audit_events for %s: %v", tid, err)
				}
				if _, err := fixtureDB.ExecContext(bg,
					`DELETE FROM audit_chain_heads WHERE tenant_id = @p1`, tid.String()); err != nil {
					t.Errorf("cleanup audit_chain_heads for %s: %v", tid, err)
				}
				if _, err := fixtureDB.ExecContext(bg,
					`DELETE FROM admin.tenants WHERE tenant_id = @p1`, tid.String()); err != nil {
					t.Errorf("cleanup admin.tenants for %s: %v", tid, err)
				}
			}
		}()

		p := &Plugin{dialect: dialect, logger: quiet, config: Config{RetentionDays: 1}}
		p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

		// ARM 1 -- THE KNOWN-POSITIVE. recordAudit builds its insert context
		// from context.Background() (plugins/auditlog/migrations.go's v2
		// comment explains why: the write must survive a cancelled request),
		// so it carries no tenant unless it adds one back itself. Against the
		// policy, an insert naming no tenant is refused outright -- if this
		// arm fails with 0 rows written, the fix is in recordOnce/recordAudit,
		// not in this test.
		p.recordAudit(ctx, tenantA, "someone@example.com", "GET", "/mine", 200, "127.0.0.1", "probe", time.Millisecond)
		var mineCount int
		if err := fixtureDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE tenant_id = @p1`, tenantA.String()).Scan(&mineCount); err != nil {
			t.Fatalf("counting the row recordAudit should have written: %v", err)
		}
		if mineCount != 1 {
			t.Fatalf("recordAudit wrote %d rows for its tenant on %s, want 1.\n\n"+
				"recordAudit derives its insert context from context.Background() so the "+
				"write survives a cancelled request; if it also fails to carry the tenant "+
				"forward, the policy refuses the insert and the only symptom is an empty "+
				"table.", mineCount, be.Name)
		}

		// The negative control: tenant B's row, seeded directly so a broken
		// policy has something to leak.
		if _, err := fixtureDB.ExecContext(ctx,
			`INSERT INTO audit_events (id, tenant_id, method, path, status_code, duration_ms, timestamp)
			 VALUES (NEWID(), @p1, 'GET', '/theirs', 200, 1, SYSUTCDATETIME())`, tenantB.String()); err != nil {
			t.Fatalf("seed tenant B's row: %v", err)
		}

		// ARM 2 -- THE POLICY ACTUALLY APPLIES. This query carries no explicit
		// WHERE tenant_id at all -- it is scoped purely by plugin.ForTenant
		// setting SESSION_CONTEXT, which is exactly what the sibling
		// PostgreSQL test's arm 2 does with auth.WithTenantID (the same
		// underlying tenantctx.With -- see plugin.ForTenant's doc comment).
		// Two rows exist, one per tenant; a read scoped to tenant A must see
		// exactly one.
		var seen int
		scoped := plugin.ForTenant(ctx, tenantA)
		if err := p.db.QueryRow(scoped, `SELECT count(*) FROM audit_events`).Scan(&seen); err != nil {
			t.Fatalf("counting through the plugin adapter as tenant A on %s: %v", be.Name, err)
		}
		if seen != 1 {
			t.Errorf("on %s: a tenant-scoped read saw %d rows, want 1.\n\n"+
				"Two rows exist, one per tenant. Seeing 2 means the SECURITY POLICY is not "+
				"filtering -- either it was never created, or this connection bypasses it, "+
				"which SQL Server's policy should never do for an ordinary principal.",
				be.Name, seen)
		}

		// ARM 3 -- RETENTION REACHES BOTH TENANTS WITHOUT A BYPASS. Age both
		// rows past the cutoff and let cleanupRetention run its real sweep:
		// expiredTenants finds them via plugin.AcrossAllTenants, and
		// retainTenant deletes each tenant's rows under its own
		// plugin.ForTenant. A sweep that only reaches the tenant that happens
		// to run first, or that the policy silently narrows to nothing, both
		// read as success unless the count is checked.
		if _, err := fixtureDB.ExecContext(ctx,
			`UPDATE audit_events SET [timestamp] = DATEADD(day, -30, SYSUTCDATETIME()) WHERE tenant_id IN (@p1, @p2)`,
			tenantA.String(), tenantB.String()); err != nil {
			t.Fatalf("ageing both rows past the retention cutoff on %s: %v", be.Name, err)
		}
		deleted, err := p.cleanupRetention(ctx)
		if err != nil {
			t.Fatalf("the retention sweep failed on %s: %v\n\n"+
				"If this is a SESSION_CONTEXT/session-context error, retainTenant is not "+
				"carrying plugin.ForTenant into its deletes.", be.Name, err)
		}
		if deleted != 2 {
			t.Errorf("on %s: the retention sweep deleted %d rows, want 2 (one per tenant).\n\n"+
				"A sweep that reaches only its own tenant, or that the policy silently "+
				"narrows to nothing, both report success unless this count is checked.",
				be.Name, deleted)
		}
	}
}
