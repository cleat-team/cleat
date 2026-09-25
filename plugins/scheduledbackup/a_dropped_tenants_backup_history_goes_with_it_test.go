package scheduledbackup

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestBackupRowsSurviveATenantDrop is the inverse of what this file asserted
// before cleat#2247: cleat#2234's fix made backup_history.config_id CASCADE
// so a tenant's backup rows were deleted ALONG WITH the tenant by
// admin.drop_tenant, rather than orphaned by a foreign-key-violating DELETE
// that rolled the whole drop back.
//
// cleat#2247's v4 migration removed tenant_id from backup_config and
// backup_history entirely, and flipped their admin.plugin_tables rows to
// tenant_scoped = false (see migrations.go's v4 comment): backup
// configuration is operator-only now, so there is no tenant dimension left
// for admin.drop_tenant's sweep to reach these tables THROUGH. They are no
// longer swept at all, by any tenant's drop.
//
// So the property this test proves is the opposite of #2234's: a backup
// config and its history rows must NOT be touched by dropping a tenant --
// there is nothing tenant-specific about them left to drop. If a future
// change re-adds a tenant dimension without updating this file, backup
// history disappearing out from under a live schedule (because some OTHER
// tenant happened to get dropped) is exactly the silent-data-loss shape
// cleat#1289 and cleat#2234 both already cost time to fix once.
func TestBackupRowsSurviveATenantDrop(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect == testutil.DialectMySQL {
			// MySQL has no admin.drop_tenant at all (tiers.yaml D1: MySQL's
			// tier-1 commitment is single-tenant), so there is nothing for
			// this test to exercise on that dialect -- matching the sibling
			// tests this file used to be modeled on.
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

			victim := uuid.New()
			if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect,
				`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
				victim, "backup-survives-drop-"+victim.String()[:8]); err != nil {
				t.Fatalf("seed admin.tenants: %v", err)
			}

			// Seeded through be.DB directly, not a tenant-scoped context:
			// there is no tenant_id column left to scope by, and no policy
			// left on either table (v4 dropped both).
			configID := uuid.New()
			now := time.Now()
			if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect, `
				INSERT INTO backup_config (id, name, cron, retention_days, enabled, created_at, updated_at)
				VALUES ($1, $2, $3, $4, true, $5, $5)
			`, configID, "nightly", "0 2 * * *", 30, now); err != nil {
				t.Fatalf("seed backup_config: %v", err)
			}
			historyID := uuid.New()
			if _, err := plugintest.ExecRebound(t, ctx, be.DB, dialect, `
				INSERT INTO backup_history (id, config_id, filename, size_bytes, status, started_at, completed_at, created_at)
				VALUES ($1, $2, $3, $4, 'completed', $5, $5, $5)
			`, historyID, configID, "backup-1.dump", int64(1024), now); err != nil {
				t.Fatalf("seed backup_history: %v", err)
			}

			countByID := func(table string, id uuid.UUID) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, be.DB, dialect,
					`SELECT count(*) FROM `+table+` WHERE id = $1`,
					id).Scan(&n); err != nil {
					t.Fatalf("count %s: %v", table, err)
				}
				return n
			}

			if got := countByID("backup_config", configID); got != 1 {
				t.Fatalf("PRECONDITION FAILED: backup_config has %d rows for %s before the drop, want 1", got, configID)
			}
			if got := countByID("backup_history", historyID); got != 1 {
				t.Fatalf("PRECONDITION FAILED: backup_history has %d rows for %s before the drop, want 1", got, historyID)
			}

			switch be.Dialect {
			case testutil.DialectMSSQL:
				if _, err := be.DB.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
					t.Fatalf("admin.drop_tenant: %v", err)
				}
			default:
				if _, err := be.DB.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
					t.Fatalf("admin.drop_tenant: %v", err)
				}
			}

			// Prove the drop actually ran, not merely that nothing raised.
			var tenantRow int
			if err := plugintest.QueryRowRebound(t, ctx, be.DB, dialect,
				`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`,
				victim).Scan(&tenantRow); err != nil {
				t.Fatalf("count admin.tenants: %v", err)
			}
			if tenantRow != 0 {
				t.Fatal("PRECONDITION FAILED: admin.drop_tenant left the tenant's admin.tenants row behind, " +
					"so it did not run to completion and the counts below say nothing")
			}

			if got := countByID("backup_config", configID); got != 1 {
				t.Errorf("backup_config row was removed by admin.drop_tenant (count=%d), want 1 -- "+
					"it is operator-only and no longer belongs to any tenant (cleat#2247)", got)
			}
			if got := countByID("backup_history", historyID); got != 1 {
				t.Errorf("backup_history row was removed by admin.drop_tenant (count=%d), want 1 -- "+
					"it is operator-only and no longer belongs to any tenant (cleat#2247)", got)
			}
		})
	}
}
