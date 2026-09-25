package scheduledbackup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// processed on this connection"): the claim transaction still commits, but
// next_run_at is never advanced by it, so the config is still due and a
// second, later poll -- before the in-flight backup finishes and its own
// completion-time updateNextRun call papers over the gap -- claims and
// dispatches it again. On MySQL the failed Exec leaves the connection
// unusable ("driver: bad connection"), so the transaction's own Commit
// fails and runDueBackups returns before ever reaching the dispatch loop --
// nothing is dispatched at all on that poll.
//
// A fake, blocking pg_dump is load-bearing here, not incidental. Without
// one, pg_dump is simply absent from this machine and every CI runner (ci.yml
// installs no postgresql-client), so executeScheduledBackup's own failure
// path runs -- and it, too, calls updateNextRun (see its comment), which
// silently repairs next_run_at regardless of whether the claim transaction's
// own advance ever took effect. That fallback made the very first version of
// this test pass on PostgreSQL even with the bug reintroduced: a first poll
// dispatched fine, and by the time a *second* poll ran (after waiting for
// the first backup to finish), the fallback had already fixed next_run_at.
// Blocking pg_dump until this test says so removes that race: the second
// poll runs while the first backup's completion-time fixup provably cannot
// have happened yet, because the fake process is still parked on a file that
// does not exist until the test creates it.
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

			release := filepath.Join(t.TempDir(), "release")
			installFakePgDump(t, fmt.Sprintf(
				"while [ ! -f %q ]; do sleep 0.02; done\ntouch \"$2\"\n", release))

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

			// Poll 1: claims both configs, commits (whether or not it
			// advanced next_run_at), and dispatches two goroutines that
			// each block inside the fake pg_dump.
			p.runDueBackups(ctx)

			// Poll 2, immediately, with no wait: the fake pg_dump cannot
			// have completed (the release file does not exist yet), so
			// neither config's completion-time updateNextRun can have run.
			// This can only find either config due again if poll 1's claim
			// transaction failed to advance next_run_at under its own lock.
			p.runDueBackups(ctx)

			// Now let both polls' backups actually finish.
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatalf("releasing fake pg_dump: %v", err)
			}
			waitForBgBackups(t, p)

			for _, id := range []uuid.UUID{configID, configID2} {
				if got := countBackupHistory(t, db, dialect, id); got != 1 {
					t.Fatalf("after two immediately-consecutive runDueBackups polls: %d backup_history "+
						"rows for config %s, want exactly 1 -- the second poll re-claimed and "+
						"re-dispatched a config the first poll's transaction should already have "+
						"advanced past due", got, id)
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
