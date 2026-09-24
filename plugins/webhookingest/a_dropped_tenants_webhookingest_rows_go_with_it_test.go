package webhookingest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestADroppedTenantsWebhookIngestRowsGoWithIt pins the thing cleat#2199's
// design note assumed rather than merely fixed: admin.drop_tenant iterates
// admin.plugin_tables ordered by (schema_name, table_name)
// (migrations/postgres/082_..., migrations/mssql/074_..., both the latest
// CREATE OR REPLACE for the function on their dialect), and
// "webhook_events" < "webhook_sources" alphabetically, so the child table is
// always emptied before the parent. That ordering is what stops
// webhook_events.source_id's missing ON DELETE action from turning a
// tenant drop into the same FK failure cleat#2199 fixed for a single
// source delete -- but it was never actually exercised by any test with a
// webhookingest event present until this one. Read the stored procedure to
// find the ordering; this proves it rather than trusting the reading.
//
// PostgreSQL and SQL Server only. MySQL has no admin.drop_tenant at all --
// no stored routine defines it, and cmd/cleatctl/droptenant.go has no MySQL
// dispatch -- consistent with tiers.yaml's D1 guard: cleat's multi-tenant
// story is PostgreSQL/SQL Server, and MySQL's tier-1 commitment is
// single-tenant. A pre-existing gap, not something this test can exercise or
// this PR changes.
func TestADroppedTenantsWebhookIngestRowsGoWithIt(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect == testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			victim := uuid.New()
			bystander := uuid.New()

			for _, tn := range []uuid.UUID{victim, bystander} {
				if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, dialect),
					tn, "drop-tenant-webhookingest-"+tn.String()[:8]); err != nil {
					t.Fatalf("seed admin.tenants for %s: %v", tn, err)
				}

				// Through p.db under a tenant-scoped context, not be.DB
				// directly: both tables carry a row-level policy since
				// cleat#1992, and a raw connection with no session context is
				// BLOCKED outright on SQL Server (its BLOCK predicate refuses
				// the write; see a_retry_is_stamped_with_the_database_clock_
				// multidb_test.go for the same shape).
				seedCtx := plugin.ForTenant(ctx, tn)
				sourceID := uuid.New()
				if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
					INSERT INTO webhook_sources (tenant_id, id, name, enabled)
					VALUES ($1, $2, $3, $4)
				`, dialect), tn, sourceID, "drop-tenant-probe", true); err != nil {
					t.Fatalf("seed webhook_sources for %s: %v", tn, err)
				}
				if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
					INSERT INTO webhook_events (id, source_id, tenant_id, received_at)
					VALUES ($1, $2, $3, $4)
				`, dialect), uuid.New(), sourceID, tn, time.Now()); err != nil {
					t.Fatalf("seed webhook_events for %s: %v", tn, err)
				}
			}

			// Every count below goes through a connection that can see EVERY
			// tenant's rows in a TenantScoped plugin table: a no-op pool read
			// on PostgreSQL (superuser bypasses RLS unconditionally) and a
			// cross_tenant-pinned connection on SQL Server (where sysadmin/dbo
			// are still subject to the policy). A read that could instead be
			// silently FILTERED by the very thing under test would make a
			// zero count after the drop mean nothing.
			readConn := be.CrossTenantConn(t, ctx, "cleat#2199: counting webhookingest rows across two tenants around a drop")
			countSources := func(tn uuid.UUID) int {
				t.Helper()
				var n int
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT count(*) FROM webhook_sources WHERE tenant_id = $1`, dialect),
					tn).Scan(&n); err != nil {
					t.Fatalf("count webhook_sources for %s: %v", tn, err)
				}
				return n
			}
			countEvents := func(tn uuid.UUID) int {
				t.Helper()
				var n int
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT count(*) FROM webhook_events WHERE tenant_id = $1`, dialect),
					tn).Scan(&n); err != nil {
					t.Fatalf("count webhook_events for %s: %v", tn, err)
				}
				return n
			}

			// A table that was never seeded and a table that was correctly
			// emptied both count zero afterward (cleat#1265) -- so the
			// preconditions have to be checked before the drop, not inferred
			// from the result.
			if got := countSources(victim); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim has %d webhook_sources rows before the drop, want 1", got)
			}
			if got := countEvents(victim); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim has %d webhook_events rows before the drop, want 1", got)
			}
			if got := countSources(bystander); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander has %d webhook_sources rows before the drop, want 1", got)
			}
			if got := countEvents(bystander); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander has %d webhook_events rows before the drop, want 1", got)
			}

			// The call under test. Failing here (a foreign key violation) is
			// exactly the regression this test exists to catch: it means
			// admin.plugin_tables' ordering stopped emptying webhook_events
			// before webhook_sources, and a tenant with any ingested webhook
			// history could no longer be dropped at all.
			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"admin.plugin_tables' drop order (schema_name, table_name) no longer empties "+
						"webhook_events before webhook_sources", err)
				}
			default:
				if _, err := be.DB.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"admin.plugin_tables' drop order (schema_name, table_name) no longer empties "+
						"webhook_events before webhook_sources", err)
				}
			}

			// Prove the sweep actually ran, not merely that nothing raised: a
			// drop_tenant that silently did nothing leaves the same surviving
			// rows and would read as the same bug (cleat#1265).
			var tenantRow int
			if err := be.DB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, dialect),
				victim).Scan(&tenantRow); err != nil {
				t.Fatalf("count admin.tenants: %v", err)
			}
			if tenantRow != 0 {
				t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row " +
					"behind, so it did not run to completion and the counts below say nothing")
			}

			if got := countSources(victim); got != 0 {
				t.Errorf("victim's webhook_sources rows survived admin.drop_tenant (count=%d)", got)
			}
			if got := countEvents(victim); got != 0 {
				t.Errorf("victim's webhook_events rows survived admin.drop_tenant (count=%d)", got)
			}
			if got := countSources(bystander); got != 1 {
				t.Errorf("dropping the victim changed the bystander's webhook_sources rows (count=%d, want 1)", got)
			}
			if got := countEvents(bystander); got != 1 {
				t.Errorf("dropping the victim changed the bystander's webhook_events rows (count=%d, want 1)", got)
			}
		})
	}
}
