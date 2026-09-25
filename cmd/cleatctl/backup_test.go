package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/scheduledbackup"
	"github.com/google/uuid"
)

// TestBackupStatementsRebindPerDialect is TestQuotaStatementsRebindPerDialect's
// counterpart for this file (quota_test.go), and the same check: a statement
// written in the portable $N form that happened to work on postgres because
// nobody rewrote its placeholders would pass every other test here, since
// TestBackupCommandWorksOnEveryDialect drives postgres through the same
// d.rebind call and would not notice a no-op.
//
// backupConfigListSQL carries no $N at all (it lists every row, unfiltered),
// same as quotaListAllSQL's own exemption, and is not in this list for that
// reason.
func TestBackupStatementsRebindPerDialect(t *testing.T) {
	for _, q := range []string{
		backupConfigCreateSQL,
		backupConfigGetByIDSQL,
		backupConfigGetByNameSQL,
		backupConfigDeleteSQL,
		backupConfigRequestRunSQL,
	} {
		if !strings.Contains(q, "$1") {
			continue
		}
		pg := dialectPostgres.rebind(q)
		if !strings.Contains(pg, "$1") {
			t.Errorf("dialectPostgres.rebind(%q) = %q, want it to still contain $1", q, pg)
		}
		my := dialectMySQL.rebind(q)
		if strings.Contains(my, "$1") || !strings.Contains(my, "?") {
			t.Errorf("dialectMySQL.rebind(%q) = %q, want no $1 and a ?", q, my)
		}
		ms := dialectMSSQL.rebind(q)
		if strings.Contains(ms, "$1") || !strings.Contains(ms, "@p1") {
			t.Errorf("dialectMSSQL.rebind(%q) = %q, want no $1 and a @p1", q, ms)
		}
	}
}

// TestBackupHistoryQueriesRebindPerDialect is the plugin.Query counterpart of
// the check above: backupHistoryListSQL/backupHistoryListByConfigSQL each
// carry a dialect-specific MSSQL arm (LIMIT is not valid T-SQL), so this
// checks each dialect's OWN arm rebinds correctly, rather than assuming one
// arm's rebind behavior for all three the way a plain string could.
func TestBackupHistoryQueriesRebindPerDialect(t *testing.T) {
	for _, q := range []struct {
		name string
		q    plugin.Query
	}{
		{"backupHistoryListSQL", backupHistoryListSQL},
		{"backupHistoryListByConfigSQL", backupHistoryListByConfigSQL},
	} {
		pg := dialectPostgres.rebind(q.q.For(dialectPostgres.query))
		if !strings.Contains(pg, "$1") {
			t.Errorf("%s: postgres arm = %q, want it to still contain $1", q.name, pg)
		}
		if !strings.Contains(pg, "LIMIT") {
			t.Errorf("%s: postgres arm = %q, want LIMIT (the Default arm)", q.name, pg)
		}
		my := dialectMySQL.rebind(q.q.For(dialectMySQL.query))
		if strings.Contains(my, "$1") || !strings.Contains(my, "?") {
			t.Errorf("%s: mysql arm = %q, want no $1 and a ?", q.name, my)
		}
		ms := dialectMSSQL.rebind(q.q.For(dialectMSSQL.query))
		if strings.Contains(ms, "$1") || !strings.Contains(ms, "@p1") {
			t.Errorf("%s: mssql arm = %q, want no $1 and a @p1", q.name, ms)
		}
		if strings.Contains(ms, "LIMIT") {
			t.Errorf("%s: mssql arm = %q, still carries LIMIT -- SQL Server rejects it outright", q.name, ms)
		}
		if !strings.Contains(ms, "FETCH NEXT") {
			t.Errorf("%s: mssql arm = %q, want its own FETCH NEXT arm rather than falling back to Default", q.name, ms)
		}
	}
}

// TestBackupCommandWorksOnEveryDialect is the empirical check behind this
// command's portedOn entry (cmd/cleatctl/ported.go). backup_config and
// backup_history carry no admin.-qualified SQL (unlike drop-tenant) and no
// row-level security to route a connection around as of migration v4
// (cleat#2247 dropped the column and the policy on every dialect) -- every
// statement in backup.go is $N-shaped and rewritten through d.rebind, the
// same convention TestSlackWorkspaceStatementsRebindPerDialect and
// TestQuotaStatementsRebindPerDialect pin for their own files. This proves
// the claim end to end rather than by resemblance to those two.
func TestBackupCommandWorksOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
		d    dialect
	}{
		{"postgres", testutil.DialectPostgres, dialectPostgres},
		{"mysql", testutil.DialectMySQL, dialectMySQL},
		{"mssql", testutil.DialectMSSQL, dialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()

			loaded := []*plugin.LoadedPlugin{{Plugin: scheduledbackup.New(), Healthy: true}}
			if err := plugin.RunMigrations(ctx, db, tc.d.query, nil, loaded); err != nil {
				t.Fatalf("apply scheduledbackup migrations on %s: %v", tc.name, err)
			}

			name := "wsdt-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
			if !scheduledbackup.ValidConfigName(name) {
				t.Fatalf("test config name %q is not itself valid: %s", name, scheduledbackup.ConfigNameRule)
			}

			// config-create.
			runBackupConfigCreate(ctx, db, tc.d, []string{
				"--name", name, "--cron", "0 0 * * *", "--retention-days", "7",
			})
			id, err := resolveConfigID(ctx, db, tc.d, "", name)
			if err != nil {
				t.Fatalf("resolveConfigID after create: %v", err)
			}

			// config-list must find it, enabled by default (no --disabled given).
			var enabled bool
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT enabled FROM backup_config WHERE id = $1`), id).Scan(&enabled); err != nil {
				t.Fatalf("reading enabled after create: %v", err)
			}
			if !enabled {
				t.Fatal("a config created with no --disabled must be enabled")
			}
			var retentionDays int
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT retention_days FROM backup_config WHERE id = $1`), id).Scan(&retentionDays); err != nil {
				t.Fatalf("reading retention_days after create: %v", err)
			}
			if retentionDays != 7 {
				t.Fatalf("retention_days = %d, want 7", retentionDays)
			}

			// config-update: disable it and change the cron.
			runBackupConfigUpdate(ctx, db, tc.d, []string{"--id", id.String(), "--cron", "0 12 * * *", "--disabled"})
			var cron string
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT cron, enabled FROM backup_config WHERE id = $1`), id).Scan(&cron, &enabled); err != nil {
				t.Fatalf("reading after update: %v", err)
			}
			if cron != "0 12 * * *" || enabled {
				t.Fatalf("after config-update --disabled, got cron=%q enabled=%v, want %q false", cron, enabled, "0 12 * * *")
			}

			// backup run is request-only: it must advance next_run_at to
			// approximately now, and must not touch backup_history or invoke
			// pg_dump (there is no DSN configured in this test at all).
			before := time.Now().UTC().Add(-time.Minute)
			runBackupRun(ctx, db, tc.d, []string{"--name", name})
			var nextRunAt time.Time
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT next_run_at FROM backup_config WHERE id = $1`), id).Scan(&nextRunAt); err != nil {
				t.Fatalf("reading next_run_at after run: %v", err)
			}
			if nextRunAt.Before(before) {
				t.Fatalf("next_run_at = %s, want it advanced to approximately now", nextRunAt)
			}

			// history reads rows this CLI never writes -- seed one the way
			// the background loop's markBackupFailed/completion path would,
			// then confirm `backup history` finds it by config id and by
			// wildcard.
			historyID := uuid.New()
			started := time.Now().UTC().Add(-time.Hour)
			if _, err := db.ExecContext(ctx, tc.d.rebind(`
				INSERT INTO backup_history (id, config_id, filename, status, started_at)
				VALUES ($1, $2, $3, $4, $5)
			`), historyID, id, "backup-"+name+".sql.gz", "completed", started); err != nil {
				t.Fatalf("seeding backup_history: %v", err)
			}

			stdout, stderr := withExitPanicOutput(t, func() {
				runBackupHistory(ctx, db, tc.d, []string{"--id", id.String()})
			})
			if !strings.Contains(stdout, historyID.String()) {
				t.Errorf("backup history --id %s did not list seeded row %s:\nstdout:\n%s\nstderr:\n%s", id, historyID, stdout, stderr)
			}
			stdoutAll, stderrAll := withExitPanicOutput(t, func() {
				runBackupHistory(ctx, db, tc.d, nil)
			})
			if !strings.Contains(stdoutAll, historyID.String()) {
				t.Errorf("backup history (no filter) did not list seeded row %s:\nstdout:\n%s\nstderr:\n%s", historyID, stdoutAll, stderrAll)
			}

			// config-delete must remove the config but leave its history row,
			// per this file's own doc comment ("its backup_history rows are
			// unaffected").
			runBackupConfigDelete(ctx, db, tc.d, []string{"--id", id.String()})
			var stillThere int
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT count(*) FROM backup_config WHERE id = $1`), id).Scan(&stillThere); err != nil {
				t.Fatalf("checking config deletion: %v", err)
			}
			if stillThere != 0 {
				t.Fatal("backup config-delete left the config row behind")
			}
			var historyStillThere int
			if err := db.QueryRowContext(ctx, tc.d.rebind(`SELECT count(*) FROM backup_history WHERE id = $1`), historyID).Scan(&historyStillThere); err != nil {
				t.Fatalf("checking history survival: %v", err)
			}
			if historyStillThere != 1 {
				t.Fatal("backup config-delete removed a backup_history row it must leave alone")
			}
		})
	}
}
