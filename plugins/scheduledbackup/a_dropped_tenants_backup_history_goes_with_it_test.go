package scheduledbackup

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
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestADroppedTenantsBackupHistoryGoesWithIt is cleat#2234's own test.
// admin.drop_tenant sweeps every TenantScoped plugin table by tenant_id, in
// NAME order -- migrations/postgres/082 and migrations/mssql/074 both walk
// their registries ORDER BY schema_name, table_name (or sys.tables' own name
// order). backup_config sorts before backup_history, so the sweep deletes the
// parent first. Before migrations.go v3 added ON DELETE CASCADE to
// backup_history.config_id (v1, no ON DELETE action), that DELETE hit a
// foreign key violation for any tenant with backup history and rolled back
// the WHOLE drop -- confirmed by cleat-review with a victim tenant carrying
// one backup_config row and one backup_history row: admin.tenants,
// backup_config and backup_history all survived at 1/1/1, and a bystander
// tenant with a config but no history dropped cleanly (the FK is only
// reached when there is a child row to violate it).
//
// This proves the CASCADE is both necessary and sufficient: no other code
// change touches the drop-tenant path itself.
//
// PostgreSQL and SQL Server only, matching notifications' and webhookingest's
// own versions of this test: MySQL has no admin.drop_tenant at all -- no
// stored routine defines it, and cmd/cleatctl/droptenant.go has no MySQL
// dispatch -- consistent with tiers.yaml's D1 guard, MySQL's tier-1
// commitment being single-tenant.
func TestADroppedTenantsBackupHistoryGoesWithIt(t *testing.T) {
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

			configIDs := map[uuid.UUID]uuid.UUID{} // tenant -> backup_config id

			for _, tn := range []uuid.UUID{victim, bystander} {
				if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect,
					`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					tn, "drop-tenant-scheduledbackup-"+tn.String()[:8]); err != nil {
					t.Fatalf("seed admin.tenants for %s: %v", tn, err)
				}

				// Through p.db under a tenant-scoped context, not be.DB
				// directly: backup_config and backup_history both carry a
				// row-level policy since migrations.go v2, and a raw
				// connection with no session context is BLOCKED outright on
				// SQL Server (migration 103's BLOCK predicates).
				seedCtx := plugin.ForTenant(ctx, tn)
				configID := uuid.New()
				configIDs[tn] = configID
				now := time.Now()
				if _, err := p.db.Exec(seedCtx, `
					INSERT INTO backup_config (tenant_id, id, name, cron, s3_bucket, s3_prefix, retention_days, enabled, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8, $8)
				`, tn, configID, "nightly", "0 2 * * *", "test-bucket", "backups/", 30, now); err != nil {
					t.Fatalf("seed backup_config for %s: %v", tn, err)
				}
				if _, err := p.db.Exec(seedCtx, `
					INSERT INTO backup_history (id, config_id, tenant_id, filename, size_bytes, status, started_at, completed_at, created_at)
					VALUES ($1, $2, $3, $4, $5, 'completed', $6, $6, $6)
				`, uuid.New(), configID, tn, "backup-1.dump", int64(1024), now); err != nil {
					t.Fatalf("seed backup_history for %s: %v", tn, err)
				}
			}

			// Every count below goes through a connection that can see EVERY
			// tenant's rows in a TenantScoped plugin table: a no-op pool read
			// on PostgreSQL (superuser bypasses RLS unconditionally) and a
			// cross_tenant-pinned connection on SQL Server (where sysadmin/dbo
			// are still subject to the policy). A read that could instead be
			// silently FILTERED by the very thing under test would make a
			// zero count after the drop mean nothing.
			readConn := be.CrossTenantConn(t, ctx, "cleat#2234: counting scheduledbackup rows across two tenants around a drop")
			countBackupConfig := func(tn uuid.UUID) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
					`SELECT count(*) FROM backup_config WHERE tenant_id = $1`,
					tn).Scan(&n); err != nil {
					t.Fatalf("count backup_config for %s: %v", tn, err)
				}
				return n
			}
			countBackupHistory := func(configID uuid.UUID) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect,
					`SELECT count(*) FROM backup_history WHERE config_id = $1`,
					configID).Scan(&n); err != nil {
					t.Fatalf("count backup_history for %s: %v", configID, err)
				}
				return n
			}

			// A table that was never seeded and a table that was correctly
			// emptied both count zero afterward (cleat#1265) -- so the
			// preconditions have to be checked before the drop, not inferred
			// from the result.
			if got := countBackupConfig(victim); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim has %d backup_config rows before the drop, want 1", got)
			}
			if got := countBackupHistory(configIDs[victim]); got != 1 {
				t.Fatalf("PRECONDITION FAILED: victim's config has %d backup_history rows before the drop, want 1", got)
			}
			if got := countBackupConfig(bystander); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander has %d backup_config rows before the drop, want 1", got)
			}
			if got := countBackupHistory(configIDs[bystander]); got != 1 {
				t.Fatalf("PRECONDITION FAILED: bystander's config has %d backup_history rows before the drop, want 1", got)
			}

			// The call under test. Failing here (a foreign key violation) is
			// exactly the regression this test exists to catch: it means
			// backup_history.config_id no longer cascades, and a tenant with
			// any backup history could no longer be dropped at all --
			// cleat#2234.
			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"backup_history.config_id no longer cascades (migrations.go v3)", err)
				}
			default:
				if _, err := be.DB.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
					t.Fatalf("admin.drop_tenant(victim): %v -- if this is a foreign key error, "+
						"backup_history.config_id no longer cascades (migrations.go v3)", err)
				}
			}

			// Prove the sweep actually ran, not merely that nothing raised: a
			// drop_tenant that silently did nothing leaves the same surviving
			// rows and would read as the same bug (cleat#1265).
			var tenantRow int
			if err := plugintest.QueryRowRebound(t, ctx, be.DB, dialect,
				`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`,
				victim).Scan(&tenantRow); err != nil {
				t.Fatalf("count admin.tenants: %v", err)
			}
			if tenantRow != 0 {
				t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row " +
					"behind, so it did not run to completion and the counts below say nothing")
			}

			if got := countBackupConfig(victim); got != 0 {
				t.Errorf("victim's backup_config rows survived admin.drop_tenant (count=%d)", got)
			}
			if got := countBackupHistory(configIDs[victim]); got != 0 {
				t.Errorf("victim's backup_history rows survived admin.drop_tenant (count=%d), want 0 (CASCADE)", got)
			}
			if got := countBackupConfig(bystander); got != 1 {
				t.Errorf("dropping the victim changed the bystander's backup_config rows (count=%d, want 1)", got)
			}
			if got := countBackupHistory(configIDs[bystander]); got != 1 {
				t.Errorf("dropping the victim changed the bystander's backup_history rows (count=%d, want 1)", got)
			}

			// Drop the bystander too, now that every assertion that needed it
			// alive has run. Via admin.drop_tenant itself -- the same sweep
			// this test exists to verify -- rather than a t.Cleanup:
			// t.Cleanup callbacks run AFTER this closure returns, by which
			// point the deferred be.Cleanup() above has already closed
			// be.DB, so a cleanup registered with t.Cleanup here would
			// silently fail on a closed pool. cleat#2233 measured this
			// directly: exactly one of two tenants leaked per run with that
			// shape.
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
