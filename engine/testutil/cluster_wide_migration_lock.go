package testutil

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"time"
)

// cleat#1599. Two test processes migrating DIFFERENT scratch databases in ONE
// PostgreSQL instance race each other, because some objects a migration creates
// are CLUSTER-wide rather than per-database. Roles are the case that bites: six
// migrations create one.
//
// Observed in CI: `duplicate key value violates unique constraint
// "pg_authid_rolname_index"` while applying 023. Reproduced locally with eight
// concurrent migrations into eight fresh databases -- 4 of 8 failed, and the
// dominant error was not that one but `tuple concurrently updated`, from two
// sessions issuing ALTER ROLE or GRANT against the same catalogue row.
//
// WHY THE RUNNER'S OWN LOCK DOES NOT COVER THIS, and why migrations.go's
// concurrency note is true but narrower than it reads. migration.Runner takes a
// database-wide advisory lock, and PostgreSQL advisory locks are PER DATABASE.
// Measured rather than recalled: one session holding pg_advisory_lock(N) in
// database A leaves pg_try_advisory_lock(N) in database B returning true. So
// the runner is safe for concurrent callers against ONE database and has
// nothing to say about concurrent callers against SEVERAL.
//
// WHY THIS IS IN THE TEST HARNESS AND NOT THE RUNNER. A deployment migrates one
// database; the contention exists only because a test harness migrates many in
// one instance at once. A retry in the runner was written and measured first --
// it roughly halved the failures and did not remove them -- and fixing the
// symptom in shared production code to serve a test-only concurrency pattern is
// the wrong layer. Owner decision, 2026-09-15.
//
// The lock is taken on the instance's MAINTENANCE database, which every
// connection in the instance can name, so all of them contend for one lock
// regardless of which scratch database they are about to migrate.
const (
	// clusterMigrationLockID is arbitrary but must not collide with the
	// runner's own advisory lock id. It is namespaced by being taken on a
	// different database entirely, so a collision would need the same id AND
	// the maintenance database.
	clusterMigrationLockID = 0x1599_0001

	clusterLockTimeout = 60 * time.Second
)

// maintenanceDSN rewrites a PostgreSQL DSN to point at the instance's
// maintenance database, leaving credentials, host, port and parameters alone.
//
// Returns "" when the DSN is not a form this understands, which the caller
// treats as "no lock available" rather than as an error -- see
// withClusterMigrationLock.
func maintenanceDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if !strings.HasPrefix(u.Scheme, "postgres") {
		return ""
	}
	u.Path = "/postgres"
	return u.String()
}

// withClusterMigrationLock runs fn while holding an instance-wide advisory
// lock, so that only one process in this PostgreSQL instance applies
// migrations at a time.
//
// BEST EFFORT BY DESIGN. If the maintenance database cannot be reached -- a
// deployment where the test role may not connect to it, an unparseable DSN --
// fn runs anyway, unserialised, which is exactly today's behaviour. A test
// harness that refused to run because it could not take a lock would turn a
// flake into an outage, and the lock is an improvement rather than a
// correctness requirement.
func withClusterMigrationLock(dsn string, fn func()) {
	mdsn := maintenanceDSN(dsn)
	if mdsn == "" {
		fn()
		return
	}
	db, err := sql.Open("postgres", mdsn)
	if err != nil {
		fn()
		return
	}
	defer func() { _ = db.Close() }()

	// One connection: an advisory lock belongs to the session that took it, so
	// taking and releasing it must happen on the same connection.
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), clusterLockTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, clusterMigrationLockID); err != nil {
		fn()
		return
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		_, _ = db.ExecContext(rctx, `SELECT pg_advisory_unlock($1)`, clusterMigrationLockID)
	}()
	fn()
}
