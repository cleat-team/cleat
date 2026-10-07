package pgclusterlock

import (
	"database/sql"
	"os"
	"testing"

	// Registers the "postgres" driver that WithClusterMigrationLock opens by
	// name. This package deliberately depends on nothing but database/sql, so
	// the driver has to be pulled in here.
	_ "github.com/lib/pq"
)

// This package cannot use engine/testutil: testutil imports pgclusterlock
// (engine/testutil/migrations.go), so a test file here importing testutil is an
// import cycle in the test binary. It reads the DSN directly instead, and keeps
// TestDB's distinction rather than inventing a new one:
//
//	no DSN configured      -> t.Skip    (a genuine environmental precondition)
//	DSN set, unreachable   -> t.Fatal   (the configuration is wrong, and
//	                                     skipping would hide it)
//
// The second half matters more here than usual. With the Coverage job now
// supplying a database, a skip-on-unreachable would let that job go on
// reporting a clean table while connecting to nothing -- which is the exact
// shape this package's own history is made of.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		t.Skip("neither CLEAT_TEST_POSTGRES nor CLEAT_TEST_DB is set, so there is no PostgreSQL to test against")
	}
	db, err := sql.Open("postgres", maintenanceDSN(dsn))
	if err != nil {
		t.Fatalf("CLEAT_TEST_POSTGRES is set but its maintenance DSN could not be opened: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_POSTGRES is set but unreachable, so this is a configuration failure "+
			"rather than a reason to skip: %v", err)
	}
	return dsn
}

// tryAdvisoryLock opens a SEPARATE session on the maintenance database and
// reports whether it could take the cluster migration lock. Closing the
// connection releases it: an advisory lock belongs to its session.
func tryAdvisoryLock(t *testing.T, dsn string, id int64) bool {
	t.Helper()
	db, err := sql.Open("postgres", maintenanceDSN(dsn))
	if err != nil {
		t.Fatalf("open a second session: %v", err)
	}
	defer func() { _ = db.Close() }()

	var got bool
	if err := db.QueryRow(`SELECT pg_try_advisory_lock($1)`, id).Scan(&got); err != nil {
		t.Fatalf("pg_try_advisory_lock(%d) from a second session: %v", id, err)
	}
	return got
}

func TestMaintenanceDSNRewritesOnlyTheDatabase(t *testing.T) {
	// Everything except the path must survive: two callers with different
	// credentials on different hosts still have to contend for ONE lock, which
	// is only true if they all end up on the instance's maintenance database.
	cases := []struct{ in, want string }{
		{"postgres://u:p@h:5432/scratch?sslmode=disable", "postgres://u:p@h:5432/postgres?sslmode=disable"},
		{"postgresql://u@h/scratch", "postgresql://u@h/postgres"},
		{"postgres://u:p@h:5432/scratch", "postgres://u:p@h:5432/postgres"},
		// Not this package's business to reach, and each returns "" so the
		// caller runs fn unserialised rather than erroring.
		{"mysql://u:p@h:3306/scratch", ""},
		{"sqlserver://u:p@h:1433?database=x", ""},
		{"not a dsn at all", ""},
		{"postgres:///scratch", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := maintenanceDSN(c.in); got != c.want {
			t.Errorf("maintenanceDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The fallbacks are deliberate: an unparseable or non-PostgreSQL DSN means "no
// lock available", and fn still runs. A test harness that refused to run
// because it could not take a lock would turn a flake into an outage.
func TestFnRunsWhenNoLockCanBeTaken(t *testing.T) {
	for _, dsn := range []string{"", "not a dsn at all", "mysql://u:p@h:3306/x"} {
		ran := false
		WithClusterMigrationLock(dsn, func() { ran = true })
		if !ran {
			t.Errorf("fn did not run for DSN %q; a missing lock must not skip the work", dsn)
		}
	}
}

// THE PROPERTY THIS PACKAGE EXISTS FOR.
//
// The package's own doc says a lock held by one of two contending paths is not
// a lock, and that is only observable by contending. So: take the lock, and
// from a SECOND session ask whether it can be taken.
func TestTheLockIsHeldForTheDurationOfFn(t *testing.T) {
	dsn := requireDSN(t)

	var freeWhileRunning bool
	WithClusterMigrationLock(dsn, func() {
		freeWhileRunning = tryAdvisoryLock(t, dsn, clusterMigrationLockID)
	})

	if freeWhileRunning {
		t.Errorf("a second session acquired advisory lock %d while WithClusterMigrationLock's fn was "+
			"running, so migrations in two processes are NOT serialised.\n\n"+
			"Either the lock was never taken, or it was taken on a connection that did not outlive "+
			"fn. Both look identical from the outside: every migration succeeds and the contention "+
			"shows up as a `tuple concurrently updated` in an unrelated package's CI job.",
			clusterMigrationLockID)
	}

	// And the control in the other direction: if the lock were never released,
	// every later run in this instance would block until its 60s timeout. A
	// test that only checked the first half would pass against a lock that
	// leaked.
	//
	// HOW TO FALSIFY THIS, and the reason it is written down: releasing the
	// lock has TWO mechanisms -- the explicit `pg_advisory_unlock`, and the
	// deferred `db.Close()`, which ends the session that holds it. Measured: a
	// mutation that removes the unlock alone leaves this GREEN, because Close
	// covers for it; the assertion only goes red when BOTH are gone. So
	// "deleting the unlock does not fail this test" is not evidence that the
	// unlock is dead code -- it is evidence that the property is protected
	// twice. Delete one at a time and you will conclude the wrong thing.
	if !tryAdvisoryLock(t, dsn, clusterMigrationLockID) {
		t.Errorf("advisory lock %d was still held after WithClusterMigrationLock returned; "+
			"the lock leaks and later callers block until clusterLockTimeout", clusterMigrationLockID)
	}
}

// A distinct id must NOT be serialised, or the test above passes for the wrong
// reason -- a lock on the wrong id, or a server that refuses every advisory
// lock, would both read as contention.
func TestADifferentLockIDIsNotBlocked(t *testing.T) {
	dsn := requireDSN(t)

	var blocked bool
	WithClusterMigrationLock(dsn, func() {
		blocked = !tryAdvisoryLock(t, dsn, clusterMigrationLockID+1)
	})
	if blocked {
		t.Errorf("advisory lock %d was blocked while %d was held, so the contention test above "+
			"would pass even if the id were wrong", clusterMigrationLockID+1, clusterMigrationLockID)
	}
}
