package scheduledbackup

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestRunDueBackupsDispatchesExactlyOnceAndAdvancesNextRunAt is cleat#2291,
// found by cleat-review while reviewing #2290: runDueBackups called
// updateNextRunTx INSIDE the loop that scans the due-backups result set, so
// the second statement ran on the same transaction while its own rows were
// still open.
//
// On PostgreSQL that Exec fails outright ("there is already a query being
// processed on this connection"). On MySQL the failed Exec leaves the
// connection unusable ("driver: bad connection"). Both failures are now
// treated the same way (cleat#2291's should-fix on #2327): a config whose
// advance fails is dropped from this poll's dispatch rather than fired
// blind, so reintroducing the ordering bug alone no longer causes a double
// dispatch -- it causes the affected config to never advance and never
// dispatch at all, on every poll, on both dialects. Either way the config
// never gets the ONE dispatch it should.
//
// SQL Server is included here too, and is asserted NOT to fail: this claim
// used to rest on the shape of dueBackupsQuery's MSSQL text (no FOR UPDATE
// cursor clause, only WITH (UPDLOCK, READPAST, ROWLOCK) locking hints) --
// which is a claim about the SQL, not about the actual mechanism, which is
// whatever go-mssqldb's TDS-level result-set handling does with a second
// statement on the same *sql.Tx while a previous SELECT's rows aren't fully
// drained. cleat-review measured this independently with the real worker
// against SQL Server 2022 and found no failure; falsifying this test
// against a real MSSQL container (reintroducing the ordering bug alone)
// confirms the same thing -- the mssql subtest stays green while postgres
// and mysql both go red.
//
// No fake pg_dump is needed (unlike an earlier version of this test):
// executeScheduledBackup no longer touches next_run_at/last_run_at at all
// (cleat#2291's other should-fix on #2327, which also fixed a real bug of
// its own -- see background.go's doc comments), so next_run_at correctness
// no longer depends on when, or whether, the dispatched backup finishes.
// The two immediately-consecutive polls below are sufficient on their own:
// the second one can only find a config due again if the first poll's claim
// transaction failed to durably advance it.
//
// This drives the real dialect driver end to end -- a fake/mock DB has no
// notion of "a statement on the same connection while a result set is
// open", so TestSB_RunDueBackups (the existing table-mock test) stayed
// green throughout.
func TestRunDueBackupsDispatchesExactlyOnceAndAdvancesNextRunAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()
			dialect := plugin.Dialect(string(tc.td))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			testutil.SetupFullSchema(t, db, tc.td)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, db, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}
			p.deploymentSecrets = &fakeBackupDeploymentSecrets{dsn: testBackupDSN}
			p.config.DumpDir = t.TempDir()

			// TWO due configs, not one: with a single row, lib/pq can finish
			// delivering it (and know there are no more) before the loop
			// body ever runs, so the connection is already idle by the time
			// updateNextRunTx is called and the bug does not reproduce. A
			// second row keeps the cursor genuinely open at that moment.
			configID := uuid.New()
			configID2 := uuid.New()
			past := time.Now().Add(-time.Hour)
			mustInsertDueConfig(t, db, dialect, configID, "cleat-2291-once", past)
			mustInsertDueConfig(t, db, dialect, configID2, "cleat-2291-once-2", past)

			// Poll 1: claims and (if the advance succeeds) dispatches both
			// configs. Poll 2, immediately: can only find either config due
			// again if poll 1's claim transaction failed to advance it.
			p.runDueBackups(ctx)
			p.runDueBackups(ctx)
			waitForBgBackups(t, p)

			for _, id := range []uuid.UUID{configID, configID2} {
				if got := countBackupHistory(t, db, dialect, id); got != 1 {
					t.Fatalf("after two immediately-consecutive runDueBackups polls: %d backup_history "+
						"rows for config %s, want exactly 1 -- either poll 1's claim transaction did not "+
						"durably advance next_run_at (a second poll then re-dispatches), or poll 1's claim "+
						"failed outright and neither poll ever dispatched it", got, id)
				}
			}
		})
	}
}

// TestRunDueBackupsConcurrentWorkersProduceOneDispatch is cleat#2291's
// acceptance criterion for PostgreSQL: two workers polling at the same
// moment must still produce exactly one dispatch, via FOR UPDATE SKIP
// LOCKED -- a regression guard on the invariant the reordering in
// runDueBackups must not disturb, not a reproduction of the bug itself (the
// bug is about a LATER, non-overlapping poll; two genuinely-concurrent
// SELECT ... FOR UPDATE SKIP LOCKED transactions already cannot both see
// the same row).
func TestRunDueBackupsConcurrentWorkersProduceOneDispatch(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()
	dialect := plugin.Dialect(string(testutil.DialectPostgres))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	seedPlugin := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(ctx, db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: seedPlugin, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	configID := uuid.New()
	past := time.Now().Add(-time.Hour)
	mustInsertDueConfig(t, db, dialect, configID, "cleat-2291-concurrent", past)

	newWorker := func() *Plugin {
		p := &Plugin{dialect: dialect, logger: quiet}
		p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}
		p.deploymentSecrets = &fakeBackupDeploymentSecrets{dsn: testBackupDSN}
		p.config.DumpDir = t.TempDir()
		return p
	}
	w1, w2 := newWorker(), newWorker()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w1.runDueBackups(ctx) }()
	go func() { defer wg.Done(); w2.runDueBackups(ctx) }()
	wg.Wait()
	waitForBgBackups(t, w1)
	waitForBgBackups(t, w2)

	if got := countBackupHistory(t, db, dialect, configID); got != 1 {
		t.Fatalf("two concurrent runDueBackups pollers produced %d backup_history rows for one due config, "+
			"want exactly 1 -- FOR UPDATE SKIP LOCKED should have let only one of them claim it", got)
	}
}

// mustInsertDueConfig inserts one enabled backup_config row, due at dueAt,
// with the same column list cmd/cleatctl's own backupConfigCreateSQL uses
// (no tenant_id: dropped by v4, cleat#2247).
func mustInsertDueConfig(t *testing.T, db *sql.DB, dialect plugin.Dialect, id uuid.UUID, name string, dueAt time.Time) {
	t.Helper()
	now := time.Now()
	stmt, args, err := plugin.RebindArgs(`
		INSERT INTO backup_config
			(id, name, cron, retention_days, enabled, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		dialect, []any{id, name, "0 9 * * *", 30, true, dueAt, now, now})
	if err != nil {
		t.Fatalf("rebinding backup_config insert: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("inserting due backup_config %s: %v", name, err)
	}
}

func countBackupHistory(t *testing.T, db *sql.DB, dialect plugin.Dialect, configID uuid.UUID) int {
	t.Helper()
	stmt, args, err := plugin.RebindArgs(
		`SELECT COUNT(*) FROM backup_history WHERE config_id = $1`, dialect, []any{configID})
	if err != nil {
		t.Fatalf("rebinding backup_history count: %v", err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), stmt, args...).Scan(&n); err != nil {
		t.Fatalf("counting backup_history for %s: %v", configID, err)
	}
	return n
}
