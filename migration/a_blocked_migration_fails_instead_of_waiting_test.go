package migration_test

// A migration that cannot get its lock fails, instead of waiting forever and
// taking the worker's heartbeat with it. cleat#1775.
//
// # What was unbounded
//
// Before this, `lock_timeout` and `statement_timeout` appeared nowhere in
// migrations/, migration/, or any .go file -- measured with `search_path` as a
// positive control, so the empty result meant the term was absent rather than
// the search broken. A migration needing ACCESS EXCLUSIVE therefore waited
// indefinitely behind any conflicting lock, and an idle-in-transaction session
// is enough to produce one.
//
// The consequence is worse than a slow boot. Every worker migrates at boot, and
// a worker stuck in migrations is not heartbeating, so its runs go stale and the
// reaper collects them while the database is mid-DDL. `--max-reclaim-per-tick`
// (598712a1) bounds how many are collected per tick; its own commit message
// says the cause was left open. This is the cause.
//
// # Why there are two tests and not one
//
// "The migration failed" is not the same claim as "the migration failed BECAUSE
// of the timeout", and only the second one is evidence that the setting works.
// A run under a context deadline fails too, and fails in a way that looks
// similar from the outside. So the blocked case is run twice against the same
// held lock, changing exactly one variable:
//
//	lock timeout 1s, generous ctx   -> must fail with SQLSTATE 55P03
//	lock timeout DISABLED, ctx 3s   -> must fail, and must NOT be 55P03
//
// The second arm is the control. If it also reported 55P03 the first result
// would be worthless, because something other than the setting would be
// producing it.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/migration"
	"github.com/lib/pq"
)

// holdAccessExclusive opens a second connection, takes ACCESS EXCLUSIVE on
// table and keeps it until the test ends. It returns once the lock is provably
// held, rather than after a sleep: the migration under test has to start its
// wait while the lock is actually held, and timing that against a duration is
// the race this is written to avoid.
func holdAccessExclusive(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("hold: acquire connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("hold: begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+table+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold: lock %s: %v", table, err)
	}
	// Proof the lock is held, read from pg_locks on a THIRD connection rather
	// than asserted. Without this the test would be trusting that LOCK TABLE
	// returning means the lock is visible to other backends.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := db.QueryRow(`
			SELECT count(*) FROM pg_locks l
			JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = $1 AND l.mode = 'AccessExclusiveLock' AND l.granted`,
			table).Scan(&n)
		if err != nil {
			t.Fatalf("hold: read pg_locks: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hold: %s never showed a granted AccessExclusiveLock in pg_locks", table)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { _ = tx.Rollback(); _ = conn.Close() })
}

// lockTimeoutFixture writes a one-file migrations tree whose single migration
// needs a lock on an already-locked table.
func lockTimeoutFixture(t *testing.T, table string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, string(migration.DialectPostgres))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	sqlText := "ALTER TABLE " + table + " ADD COLUMN added_by_migration integer;\n"
	if err := os.WriteFile(filepath.Join(dir, "001_needs_the_lock.sql"), []byte(sqlText), 0o600); err != nil {
		t.Fatalf("write migration: %v", err)
	}
	return root
}

// isLockTimeout reports whether err is PostgreSQL's lock_timeout, 55P03.
// Keyed on SQLSTATE, not on message text: the text is localised and the code
// is not, and a substring match on "timeout" would also accept a
// statement_timeout or a context deadline, which is exactly the confusion the
// control arm exists to rule out.
func isLockTimeout(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "55P03"
	}
	return false
}

func TestABlockedMigrationFailsInsteadOfWaiting(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_locktimeout_test")
	if _, err := db.Exec(`CREATE TABLE contended (id integer)`); err != nil {
		t.Fatalf("create contended table: %v", err)
	}
	holdAccessExclusive(t, db, "contended")
	root := lockTimeoutFixture(t, "contended")

	t.Run("bounded: fails with 55P03", func(t *testing.T) {
		// ctx is deliberately far longer than the lock timeout, so a context
		// deadline cannot be what produces the failure.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		start := time.Now()
		err := runMigrations(t, ctx,
			migration.NewRunner(db, migration.DialectPostgres, root).
				WithLockTimeout(time.Second),
			migration.DialectPostgres)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("migration succeeded while the table was held under ACCESS EXCLUSIVE; " +
				"either the lock was not held or the migration did not need it")
		}
		if !isLockTimeout(err) {
			t.Fatalf("migration failed, but not with lock_timeout (55P03): %v.\n"+
				"The point of this test is the REASON for the failure, not the failure.", err)
		}
		if elapsed > 30*time.Second {
			t.Errorf("took %v to give up on a 1s lock timeout; the bound is not doing the work", elapsed)
		}
		t.Logf("failed with 55P03 after %v", elapsed)
	})

	t.Run("control: disabled, fails for a different reason", func(t *testing.T) {
		// Same held lock, same migration, one variable changed. If this also
		// reported 55P03, the arm above would prove nothing.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		err := runMigrations(t, ctx,
			migration.NewRunner(db, migration.DialectPostgres, root).
				WithLockTimeout(0),
			migration.DialectPostgres)
		if err == nil {
			t.Fatal("migration succeeded with the lock held and no timeout; " +
				"the fixture is not actually contended")
		}
		if isLockTimeout(err) {
			t.Fatalf("lock timeout fired with the bound DISABLED: %v.\n"+
				"Something other than WithLockTimeout is setting it, so the other "+
				"arm's 55P03 is not evidence that this setting works.", err)
		}
		t.Logf("control failed for a different reason, as required: %v", err)
	})
}

// The timeout must not ride back into the pool on a returned connection.
// Exactly the shape of TestRunner_LeavesSearchPathUnchanged, and for the same
// reason: *sql.Conn.Close() RETURNS the connection rather than closing it, so a
// session-level SET left behind would bound locks for ordinary application
// traffic on whichever caller was handed that connection next.
func TestRunner_LeavesLockTimeoutUnchanged(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_locktimeout_leak_test")
	simulateExistingDeployment(t, db)
	db.SetMaxOpenConns(1)

	var before string
	if err := db.QueryRow(`SHOW lock_timeout`).Scan(&before); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}

	if err := runMigrations(t, context.Background(),
		migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t)).
			WithLockTimeout(7*time.Second),
		migration.DialectPostgres); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var after string
	if err := db.QueryRow(`SHOW lock_timeout`).Scan(&after); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if before != after {
		t.Errorf("migrations leaked a lock_timeout onto a pooled connection: %q -> %q. "+
			"Every later query on this connection would inherit it.", before, after)
	}
	if strings.Contains(after, "7s") || after == "7000" {
		t.Errorf("the runner's own lock_timeout (%q) is still set on a pooled connection", after)
	}
}
