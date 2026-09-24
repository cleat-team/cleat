package notifications

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

// TestADroppedTenantsNotificationsRowsGoWithIt is cleat#2222's own test:
// admin.drop_tenant sweeps every TenantScoped plugin table by tenant_id
// (migrations/postgres/066_..., migrations/mssql/074_..., both read in full
// before this test was written), and webhook_config IS one -- migrations.go's
// v2 declares it TenantScoped. webhook_delivery is NOT: it carries no
// tenant_id column at all (a gap v2's own comment calls out), so neither
// dialect's sweep can reach it directly. Before migrations.go v7 added
// ON DELETE CASCADE to webhook_delivery.webhook_id, dropping a tenant with
// any webhook delivery history hit the same foreign key shape cleat#2199 fixed
// for webhookingest's source/event pair: a hard DELETE FROM webhook_config
// with dependent webhook_delivery rows still pointing at it.
//
// This proves the CASCADE is both necessary and sufficient: no other code
// change touches the drop-tenant path itself.
//
// PostgreSQL and SQL Server only, matching webhookingest's own version of this
// test (a_dropped_tenants_webhookingest_rows_go_with_it_test.go): MySQL has no
// admin.drop_tenant at all -- no stored routine defines it, and
// cmd/cleatctl/droptenant.go has no MySQL dispatch -- consistent with
// tiers.yaml's D1 guard, MySQL's tier-1 commitment being single-tenant.
func TestADroppedTenantsNotificationsRowsGoWithIt(t *testing.T) {
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

			webhookIDs := map[uuid.UUID]uuid.UUID{} // tenant -> webhook id

			for _, tn := range []uuid.UUID{victim, bystander} {
				if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, dialect),
					tn, "drop-tenant-notifications-"+tn.String()[:8]); err != nil {
					t.Fatalf("seed admin.tenants for %s: %v", tn, err)
				}

				// Through p.db under a tenant-scoped context, not be.DB
				// directly: webhook_config carries a row-level policy since
				// migrations.go v2, and a raw connection with no session
				// context is BLOCKED outright on SQL Server (migration 103's
				// BLOCK predicates, live since cleat#2226 -- see the mssql
				// admin.drop_tenant test template this was adapted from for
				// the same shape on webhookingest's tables).
				seedCtx := plugin.ForTenant(ctx, tn)
				webhookID := uuid.New()
				webhookIDs[tn] = webhookID
				now := time.Now()
				if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
					INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, true, $6, $7)
				`, dialect), tn, webhookID, "https://example.com/hook", true, `["test.event"]`, now, now); err != nil {
					t.Fatalf("seed webhook_config for %s: %v", tn, err)
				}
				if _, err := p.db.Exec(seedCtx, plugin.Rebind(`
					INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
					VALUES ($1, $2, 'test.event', '{}', 'pending', 0, $3, $3)
				`, dialect), uuid.New(), webhookID, now); err != nil {
					t.Fatalf("seed webhook_delivery for %s: %v", tn, err)
				}
			}

			// Every count below goes through a connection that can see EVERY
			// tenant's rows in a TenantScoped plugin table: a no-op pool read
			// on PostgreSQL (superuser bypasses RLS unconditionally) and a
			// cross_tenant-pinned connection on SQL Server (where sysadmin/dbo
			// are still subject to the policy). A read that could instead be
			// silently FILTERED by the very thing under test would make a
			// zero count after the drop mean nothing.
			readConn := be.CrossTenantConn(t, ctx, "cleat#2222: counting notifications rows across two tenants around a drop")
			countWebhookConfig := func(tn uuid.UUID) int {
				t.Helper()
				var n int
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT count(*) FROM webhook_config WHERE tenant_id = $1`, dialect),
					tn).Scan(&n); err != nil {
					t.Fatalf("count webhook_config for %s: %v", tn, err)
				}
				return n
			}
			countWebhookDelivery := func(webhookID uuid.UUID) int {
				t.Helper()
				var n int
				if err := readConn.QueryRowContext(ctx, plugin.Rebind(
					`SELECT count(*) FROM webhook_delivery WHERE webhook_id = $1`, dialect),
					webhookID).Scan(&n); err != nil {
					t.Fatalf("count webhook_delivery for %s: %v", webhookID, err)
				}
				return n
			}

			// A table that was never seeded and a table that was correctly
			// emptied both count zero afterward (cleat#1265) -- so the
			// preconditions have to be checked before the drop, not inferred
			// from the result.
			if got := countWebhookConfig(victim); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim has %d webhook_config rows before the drop, want 1", got)
			}
			if got := countWebhookDelivery(webhookIDs[victim]); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim's webhook has %d webhook_delivery rows before the drop, want 1", got)
			}
			if got := countWebhookConfig(bystander); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander has %d webhook_config rows before the drop, want 1", got)
			}
			if got := countWebhookDelivery(webhookIDs[bystander]); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander's webhook has %d webhook_delivery rows before the drop, want 1", got)
			}

			// The call under test. Failing here (a foreign key violation) is
			// exactly the regression this test exists to catch: it means
			// webhook_delivery.webhook_id no longer cascades, and a tenant
			// with any webhook delivery history could no longer be dropped at
			// all -- cleat#2222.
			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"webhook_delivery.webhook_id no longer cascades (migrations.go v7)", err)
				}
			default:
				if _, err := be.DB.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"webhook_delivery.webhook_id no longer cascades (migrations.go v7)", err)
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

			if got := countWebhookConfig(victim); got != 0 {
				t.Errorf("victim's webhook_config rows survived admin.drop_tenant (count=%d)", got)
			}
			if got := countWebhookDelivery(webhookIDs[victim]); got != 0 {
				t.Errorf("victim's webhook_delivery rows survived admin.drop_tenant (count=%d), want 0 (CASCADE)", got)
			}
			if got := countWebhookConfig(bystander); got != 1 {
				t.Errorf("dropping the victim changed the bystander's webhook_config rows (count=%d, want 1)", got)
			}
			if got := countWebhookDelivery(webhookIDs[bystander]); got != 1 {
				t.Errorf("dropping the victim changed the bystander's webhook_delivery rows (count=%d, want 1)", got)
			}

			// Drop the bystander too, now that every assertion that needed
			// it alive has run. Coordinator's #2233 review flagged that a
			// prior run's bystander tenant, webhook and delivery rows were
			// left behind in the shared database indefinitely -- tn is
			// fresh per run (uuid.New()), so a leftover bystander from an
			// earlier run cannot collide with THIS run's counts, but it is
			// exactly the "unbounded table is a trap for whoever writes the
			// next test" CLAUDE.md warns about. Via admin.drop_tenant
			// itself -- the same sweep this test exists to verify -- rather
			// than a t.Cleanup: t.Cleanup callbacks run AFTER this closure
			// returns, by which point the deferred be.Cleanup() above has
			// already closed be.DB, so a cleanup registered with t.Cleanup
			// here would silently fail on a closed pool (measured: exactly
			// one of the two tenants per run leaked with that shape).
			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, bystander); err != nil {
					t.Errorf("cleanup: admin.drop_tenant(bystander): %v", err)
				}
			default:
				if _, err := be.DB.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, bystander); err != nil {
					t.Errorf("cleanup: admin.drop_tenant(bystander): %v", err)
				}
			}
		})
	}
}
