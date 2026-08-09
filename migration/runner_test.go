package migration_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
)

// The Runner in this package is what every cleat-worker executes against the
// operator's database before it will serve anything -- and until these tests
// it had none at all. Nothing in the repo had ever run it against the files in
// migrations/postgres/. The two halves of the bootstrap were each covered
// separately (engine/schema_bootstrap_test.go applies the files with psql
// semantics; the runner's own logic was exercised by nothing) and the seam
// between them was where both of the bugs below lived:
//
//  1. The migration files begin with `SET search_path = public;`. A bare SET
//     is session-scoped, so it changed how the runner's own unqualified
//     `INSERT INTO schema_migrations` resolved, on the very connection that
//     had just created that table under a different search_path. Every worker
//     died at boot with 42P01. See Runner.trackingTable.
//
//  2. Four workers start at once in docker-compose.cluster.yml and all four
//     ran migrations simultaneously, racing on CREATE TABLE IF NOT EXISTS and
//     reading each other's half-applied schema. See migrationsLockKey.
//
// Both reproduce only against a database that looks like a real deployment, so
// these tests build one rather than a convenient approximation.
//
// This file (and every other test file in this directory except split_test.go
// and internal_test.go) is `package migration_test`, an external test
// package, rather than `package migration`. That is load-bearing, not a
// style choice: engine/testutil now applies migrations through
// migration.NewRunner (IMPROVEMENT-PLAN, "the tests do not run against the
// schema that ships"), so this package is itself a dependency of testutil.
// This file also depends on testutil, for its scratch-database DSN helper.
// Go refuses to build a package whose *internal* test files (same package as
// the code under test) import something that imports the package itself --
// "import cycle not allowed in test" -- and migration -> testutil ->
// migration is exactly that shape if this file stays `package migration`.
// An *external* test package (migration_test) does not have this problem:
// Go explicitly allows X_test to import something that imports X. The two
// tests that need unexported access (TestTrackingTable_QualifiedOnlyForPostgres,
// and split_test.go's splitSQL/isAllComments cases) stay behind in
// internal_test.go and split_test.go, `package migration`, which do not
// import testutil.

// migrationsRoot returns the repo's migrations/ directory (the runner appends
// the dialect subdirectory itself). Duplicated from the one in
// internal_test.go: that one is unexported to `package migration` and this
// file is a different package (migration_test), so it cannot see it. Ten
// lines of directory lookup, not the kind of copy that drifts.
func migrationsRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../migration
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Join(wd, "..", "migrations")
	if _, err := os.Stat(filepath.Join(root, "postgres")); err != nil {
		t.Fatalf("migrations/postgres not found at %s: %v", root, err)
	}
	return root
}

// postgresDSN resolves the admin connection string via
// testutil.PostgresTestDSN, so this package's env-var precedence
// (CLEAT_TEST_POSTGRES, then CLEAT_TEST_DB, then a localhost fallback) can
// never drift from engine/testutil's.
//
// configured reports whether a DSN was explicitly supplied, as opposed to
// the hardcoded localhost fallback. Before this change the function returned
// only the DSN, and newScratchDB's Ping failure always turned into a Skip --
// exactly the bug f9bce35 fixed in engine/testutil.TestDB (a service
// container with no published port made every subtest skip silently instead
// of failing, so the CI job reported green without ever connecting). This
// package predates that fix and never received it. ci.yml's test-go job,
// matrix entry "support" (which covers ./migration/...), always sets
// CLEAT_TEST_DB to a Postgres service with a published port, so an
// unreachable Ping there is a broken environment, not an absent one -- and
// this is the regression test file for the two defects that used to crash
// every cleat-worker at boot (see the file header), so silently skipping it
// is the single worst place in the repo for this bug to recur.
//
// testutil.PostgresTestDSN() itself doesn't return the "configured" bit, and
// there is no exported helper for it, so that check is replicated here
// rather than shared -- it's two os.Getenv calls, not worth adding new
// exported surface to testutil for.
func postgresDSN(t *testing.T) (dsn string, configured bool) {
	t.Helper()
	dsn = testutil.PostgresTestDSN()
	configured = os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	return dsn, configured
}

// newScratchDB creates an empty database and returns a handle to it.
//
// name must be unique per test: these run in the same package and a shared
// scratch database would make them order-dependent, which is the class of
// defect this file exists to remove rather than add.
func newScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()

	adminDSN, configured := postgresDSN(t)
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		// See postgresDSN: only skip when nobody asked for a database at all.
		// A configured-but-unreachable Postgres must fail loud, because this
		// file is the regression test for the boot-time defects described in
		// the file header, and ci.yml always configures CLEAT_TEST_DB for the
		// job that runs this package.
		if configured {
			t.Fatalf("configured postgres database at %s is unreachable: %v", redactDSN(adminDSN), err)
		}
		t.Skipf("no postgres database at %s (default DSN, none configured): %v", redactDSN(adminDSN), err)
	}

	// A previous run that was killed mid-test (a timeout, a ^C) can leave
	// connections open, and DROP DATABASE fails with 55006 while any remain.
	// Evict them rather than making every later run depend on how the last one
	// ended.
	dropScratchDB(t, admin, name)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		dropScratchDB(t, cleanup, name)
	})

	dsn, err := swapDB(adminDSN, name)
	if err != nil {
		t.Fatalf("derive scratch DSN: %v", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	return db
}

// dropScratchDB terminates any lingering backends and drops the database.
func dropScratchDB(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	_, _ = admin.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name)
	// CREATE/DROP DATABASE cannot run inside a transaction.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop scratch database %s: %v", name, err)
	}
}

func swapDB(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	return dsn
}

// simulateExistingDeployment reproduces the one property of a real cleat
// database that the bug depended on: a schema whose name matches the
// connecting role.
//
// docker-compose.cluster.yml connects as POSTGRES_USER=cleat, and
// 001_schema.sql creates a schema called "cleat". The default search_path is
// `"$user", public`, so on any database where the files have been applied once
// -- which is every real deployment, since docker-entrypoint-initdb.d applies
// them before the workers ever connect -- unqualified CREATE TABLE lands in
// the role's schema and not in public.
//
// A test that skipped this step would start from an empty database where
// "$user" does not resolve, silently exercise the public-schema path, and pass
// against the broken code.
func simulateExistingDeployment(t *testing.T, db *sql.DB) {
	t.Helper()
	var user string
	if err := db.QueryRow(`SELECT current_user`).Scan(&user); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	if _, err := db.Exec(`CREATE SCHEMA IF NOT EXISTS ` + pgQuoteIdent(user)); err != nil {
		t.Fatalf("create schema %q: %v", user, err)
	}
	var path string
	if err := db.QueryRow(`SHOW search_path`).Scan(&path); err != nil {
		t.Fatalf("show search_path: %v", err)
	}
	if !strings.Contains(path, "$user") {
		t.Fatalf("search_path is %q, want one containing \"$user\" -- this test "+
			"only reproduces the deployment condition under the default", path)
	}
}

func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// TestRunner_AppliesShippedPostgresMigrations runs the real Runner over the
// real migrations/postgres/ files against a database shaped like a deployed
// one. This is the test that fails on the unfixed runner, with
//
//	migration 001_schema.sql: record version: pq: relation
//	"schema_migrations" does not exist (42P01)
//
// which is verbatim what cleat-worker printed before refusing to start.
func TestRunner_AppliesShippedPostgresMigrations(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_runner_test")
	simulateExistingDeployment(t, db)

	r := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t))
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run against the shipped migrations failed: %v\n\n"+
			"This is the code path every cleat-worker takes at boot. If it "+
			"fails, no worker can start against a PostgreSQL deployment.", err)
	}

	// The engine's hard requirement: its objects must be reachable as public.*,
	// because that is how the SQL in engine/ names them.
	var count int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'workflow_instances'`,
	).Scan(&count); err != nil {
		t.Fatalf("query pg_tables: %v", err)
	}
	if count != 1 {
		t.Errorf("public.workflow_instances not found after migrations (count=%d)", count)
	}

	// And the bookkeeping must be somewhere a later run will look.
	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM public.schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("read public.schema_migrations: %v", err)
	}
	if applied == 0 {
		t.Error("public.schema_migrations is empty: a second run would re-apply everything")
	}
}

// TestRunner_SecondRunAppliesNothing is the operator-upgrade path: restart a
// worker against a database that is already migrated. It must be a no-op, and
// in particular must not re-run DDL under a lock while other workers serve
// traffic.
func TestRunner_SecondRunAppliesNothing(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_runner_twice_test")
	simulateExistingDeployment(t, db)

	root := migrationsRoot(t)
	ctx := context.Background()

	if err := migration.NewRunner(db, migration.DialectPostgres, root).Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	var firstApplied string
	if err := db.QueryRow(
		`SELECT max(applied_at)::text FROM public.schema_migrations`).Scan(&firstApplied); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}

	if err := migration.NewRunner(db, migration.DialectPostgres, root).Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	var secondApplied string
	if err := db.QueryRow(
		`SELECT max(applied_at)::text FROM public.schema_migrations`).Scan(&secondApplied); err != nil {
		t.Fatalf("read applied_at after second run: %v", err)
	}
	if firstApplied != secondApplied {
		t.Errorf("second Run re-applied migrations: applied_at moved from %s to %s",
			firstApplied, secondApplied)
	}
}

// TestRunner_ConcurrentRunsAreSerialised starts several runners at the same
// instant against one empty database, which is exactly what
// docker-compose.cluster.yml does with its four workers.
//
// Unserialised, this fails intermittently with either
//
//	duplicate key value violates unique constraint "pg_type_typname_nsp_index"
//	relation "tenant_api_keys" does not exist
//
// depending on which pair of statements happens to interleave. Both appeared
// in CI. Every runner must succeed: a worker that loses this race exits.
func TestRunner_ConcurrentRunsAreSerialised(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_runner_concurrent_test")
	simulateExistingDeployment(t, db)

	const runners = 4
	// Each runner gets its own pool, as separate worker processes would.
	root := migrationsRoot(t)

	var wg sync.WaitGroup
	errs := make([]error, runners)
	start := make(chan struct{})
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = migration.NewRunner(db, migration.DialectPostgres, root).Run(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent runner %d failed: %v", i, err)
		}
	}

	// Whoever won, the result must be one clean schema, not a partial one.
	var missing []string
	for _, tbl := range []string{"workflow_instances", "workflow_defs", "event_history", "schema_migrations"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("query pg_tables: %v", err)
		}
		if n != 1 {
			missing = append(missing, tbl)
		}
	}
	if len(missing) > 0 {
		t.Errorf("public schema incomplete after concurrent runs, missing: %v", missing)
	}
}

// TestRunner_LeavesSearchPathUnchanged pins the pool hygiene fix. Migration
// files SET search_path, and without a reset the connection they ran on goes
// back into the pool resolving unqualified names differently from every other
// connection in it.
func TestRunner_LeavesSearchPathUnchanged(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_runner_searchpath_test")
	simulateExistingDeployment(t, db)

	// One connection only, so the connection the runner used is necessarily
	// the one queried afterwards.
	db.SetMaxOpenConns(1)

	var before string
	if err := db.QueryRow(`SHOW search_path`).Scan(&before); err != nil {
		t.Fatalf("show search_path: %v", err)
	}

	if err := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var after string
	if err := db.QueryRow(`SHOW search_path`).Scan(&after); err != nil {
		t.Fatalf("show search_path: %v", err)
	}
	if before != after {
		t.Errorf("migrations leaked a search_path change onto a pooled connection: %q -> %q",
			before, after)
	}
}
