package plugin

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"sync"
	"testing"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
)

// Every cleat-worker calls RunMigrations at boot, and
// docker-compose.cluster.yml starts four of them at the same instant. That is
// concurrency the function never accounted for: three of the four died with
//
//	plugin: create migrations table: pq: type "plugin_migrations" already
//	exists (42710)
//
// CREATE TABLE IF NOT EXISTS is not atomic against another session creating
// the same table -- the existence check and the catalogue insert are separate
// steps -- so "IF NOT EXISTS" buys nothing here. See lockPluginMigrations.
//
// This is the same defect, in a second copy, as the one in migration.Runner.
// Both were invisible because neither migration path had any test that ran it
// more than once at a time.

// pluginTestPostgresDSN resolves the admin connection string via
// testutil.PostgresTestDSN (CLEAT_TEST_POSTGRES, then CLEAT_TEST_DB, then a
// localhost fallback), rather than hand-duplicating that precedence.
//
// configured reports whether a DSN was explicitly supplied, as opposed to
// the hardcoded fallback. This is the same "configured but unreachable ->
// skip, not fail" bug engine/testutil.TestDB had before f9bce35 -- a service
// container with no published port made every subtest skip silently instead
// of failing, and the CI job reported green having never connected once.
// This file is the regression test for the duplicate-key migration race that
// killed 3 of 4 cleat-workers (see the file header): ci.yml's test-go job,
// matrix entry "support" (which covers ./plugin/...), always sets
// CLEAT_TEST_DB against a Postgres service with a published port, so an
// unreachable Ping there is a broken environment, not an absent one, and
// silently skipping it retires coverage for exactly the defect this file
// exists to catch.
//
// testutil has no exported helper for the "configured" bit itself, so that
// two-line env check is replicated here rather than shared.
func pluginTestPostgresDSN(t *testing.T) (dsn string, configured bool) {
	t.Helper()
	dsn = testutil.PostgresTestDSN()
	configured = os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	return dsn, configured
}

// redactDSN strips the password from a DSN before it can appear in test
// output. testutil's equivalent is unexported, so this is replicated rather
// than shared -- the same duplication already exists in
// migration/runner_test.go.
func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	return dsn
}

// newPluginScratchDB creates an empty database for one test.
func newPluginScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()

	adminDSN, configured := pluginTestPostgresDSN(t)
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		// See pluginTestPostgresDSN: only skip when nobody asked for a
		// database at all. A configured-but-unreachable Postgres must fail
		// loud here -- this is the regression test for the duplicate-key
		// migration race described in the file header, and ci.yml always
		// configures CLEAT_TEST_DB for the job that runs this package.
		if configured {
			t.Fatalf("configured postgres database at %s is unreachable: %v", redactDSN(adminDSN), err)
		}
		t.Skipf("no postgres database at %s (default DSN, none configured): %v", redactDSN(adminDSN), err)
	}

	drop := func(db *sql.DB) {
		_, _ = db.Exec(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			name)
		if _, err := db.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Fatalf("drop scratch database %s: %v", name, err)
		}
	}
	drop(admin)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		drop(cleanup)
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	return db
}

// TestRunMigrations_ConcurrentCallersAreSerialised runs RunMigrations from
// several goroutines at once against one empty database, which is what a
// multi-worker deployment does. All of them must succeed: a worker whose
// migrations fail exits rather than serving traffic.
func TestRunMigrations_ConcurrentCallersAreSerialised(t *testing.T) {
	db := newPluginScratchDB(t, "cleat_plugin_migrations_concurrent_test")

	// A core migration that races the same way real ones do.
	core := []Migration{{
		Version: 1,
		Up:      `CREATE TABLE IF NOT EXISTS plugin_concurrency_probe (id INT PRIMARY KEY)`,
	}}

	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = RunMigrations(context.Background(), db, DialectPostgres, core, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent RunMigrations caller %d failed: %v", i, err)
		}
	}

	for _, tbl := range []string{"plugin_migrations", "plugin_concurrency_probe"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("query pg_tables: %v", err)
		}
		if n != 1 {
			t.Errorf("table %s missing after concurrent migration runs", tbl)
		}
	}
}
