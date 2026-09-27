package testutil

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// SuiteTestDB returns a PostgreSQL connection to a database used only by the
// named suite, creating it and applying the shipped migrations on first use.
//
// This is IMPROVEMENT-PLAN 2.60d's isolation half, and the reason it exists is
// that the cleanup helpers issue unqualified `DELETE FROM` across every table.
// That is correct inside a database only one suite uses and catastrophic in a
// shared one: `go test` runs packages in parallel by default, so one suite's
// teardown deletes another's fixtures mid-test. The failures are
// timing-dependent -- a workflow that vanishes between two statements, a
// foreign key to a definition deleted since it was deployed -- so they read as
// flakes rather than as one cause, and the standing workaround is `-p 1`.
//
// The approach is not new here. tests/crash already had to take a database of
// its own for exactly this reason (see ensureCrashDatabase, which records the
// same diagnosis); this generalises it so other suites do not each reinvent it.
// It makes the ~74 existing cleanup call sites correct without editing any of
// them, which is why it was chosen over tenant-scoping every delete: a call
// site that gets a tenant wrong fails silently, whereas a wrong DSN fails at
// once.
//
// suite names the caller, e.g. "testutil". It becomes part of a database name,
// so it is restricted to characters that need no quoting.
func SuiteTestDB(t *testing.T, suite string) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}
	if !validSuiteName.MatchString(suite) {
		t.Fatalf("suite name %q must match %s -- it becomes part of a database "+
			"name and is not quoted", suite, validSuiteName)
	}

	dbName := suiteDatabaseName(suite)
	base := PostgresTestDSN()

	// An unreachable database is only a reason to skip when nobody asked for
	// one. If a DSN was configured explicitly, failing to connect is a failure
	// of the configuration and skipping hides it -- the Multi-DB workflow once
	// set CLEAT_TEST_POSTGRES for a service container whose port it had not
	// published, and every PostgreSQL subtest skipped itself while the job
	// reported green for its entire existence. Same treatment as TestDB, and
	// the reason scripts/check-skips.sh rejected the t.Skipf this replaced.
	configured := os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	unavailable := t.Skipf
	reason := "no PostgreSQL at %s (default DSN, none configured): %v"
	if configured {
		unavailable = t.Fatalf
		reason = "configured PostgreSQL at %s is unreachable: %v"
	}

	admin, err := sql.Open("postgres", base)
	if err != nil {
		unavailable(reason, redactDSN(base), err)
		return nil
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		unavailable(reason, redactDSN(base), err)
		return nil
	}

	ensureDatabase(t, admin, dbName, base)

	dsn, err := swapDatabaseName(base, dbName)
	if err != nil {
		t.Fatalf("building a DSN for %s: %v", dbName, err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", dbName, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to %s: %v", dbName, err)
	}

	// The migrations are idempotent and keep their own schema_migrations
	// table, so this is cheap on the runs after the first.
	applyPostgresSchemaFile(t, db)
	return db
}

var validSuiteName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// ensureDatabase creates dbName if it is absent.
//
// Deliberately not "drop and recreate": the migrations are the expensive part,
// and a suite that wants a clean slate has the cleanup helpers for that. It is
// also not an error for the database to already exist -- two test binaries can
// reach this concurrently, and losing that race is normal.
func ensureDatabase(t *testing.T, admin *sql.DB, dbName, base string) {
	t.Helper()

	var exists bool
	if err := admin.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
		t.Fatalf("checking for the %s database: %v", dbName, err)
	}
	if exists {
		return
	}

	// CREATE DATABASE cannot be parameterised. dbName is built from a name
	// this function has already constrained to [a-z0-9_], so there is nothing
	// here to inject.
	if _, err := admin.Exec(`CREATE DATABASE ` + dbName); err != nil {
		// A concurrent binary may have won the race; re-check rather than fail.
		if err2 := admin.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err2 != nil || !exists {
			t.Fatalf("creating the %s database: %v\n\n"+
				"This needs a role with CREATEDB. The connection is %s. A suite "+
				"database is how 2.60d keeps one package's unqualified DELETE "+
				"out of another package's fixtures; there is deliberately no "+
				"fallback to the shared database, because falling back silently "+
				"is the failure this is meant to remove.",
				dbName, err, redactDSN(base))
		}
	}
}

// swapDatabaseName replaces the database component of a PostgreSQL DSN.
//
// Handles both URL form (postgres://host/dbname) and keyword form
// (host=... dbname=...), because CLEAT_TEST_DB is set both ways across the
// jobs that run these suites.
func swapDatabaseName(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + name
		return u.String(), nil
	}

	// Keyword form: replace an existing dbname=, or append one.
	fields := strings.Fields(dsn)
	replaced := false
	for i, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			fields[i] = "dbname=" + name
			replaced = true
		}
	}
	if !replaced {
		fields = append(fields, "dbname="+name)
	}
	return strings.Join(fields, " "), nil
}

// suiteDatabaseName is the single place the database name is constructed.
//
// It started out used only by the test that asserts the name, with SuiteTestDB
// building the same string inline -- which the test-only-code guard flagged,
// correctly: two places deriving one name is how they drift apart, and the test
// would have gone on passing against its own copy of the rule.
func suiteDatabaseName(suite string) string { return fmt.Sprintf("cleat_test_%s", suite) }
