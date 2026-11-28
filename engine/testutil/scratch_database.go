package testutil

// cleat#3597: a database that only the calling test uses, created beside the configured one, so a
// test can have TWO real databases of one dialect: the shards of a sharded deployment.
//
// TestDB returns the one configured database, and SuiteTestDB a persistent one per suite name, so
// neither can stand in for a second shard that a test creates and throws away. A fixture that
// called TestDB twice would hold one database under two names and a "two shard" test would be
// measuring nothing. This one is created fresh, has the shipped schema applied, and is dropped
// when the test ends.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
)

var scratchDatabaseSeq atomic.Int64

// ScratchDatabase creates a database for this test alone on the server the dialect's DSN names,
// applies the shipped schema to it, and drops it at the end of the test. PostgreSQL and SQL Server
// only. prefix becomes part of the name, so it is restricted like a suite name.
//
// It needs a role that can CREATE DATABASE, as SuiteTestDB does. The skip rule is TestDB's: nothing
// configured is a reason to skip, and a configured server that cannot be reached is a failure.
func ScratchDatabase(t *testing.T, dialect Dialect, prefix string) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}
	if !validSuiteName.MatchString(prefix) {
		t.Fatalf("scratch database prefix %q must match %s -- it becomes part of a database name and is not quoted", prefix, validSuiteName)
	}
	name := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), scratchDatabaseSeq.Add(1))

	var db *sql.DB
	switch dialect {
	case DialectPostgres:
		db = createScratchPostgres(t, name)
	case DialectMSSQL:
		db = createScratchMSSQL(t, name)
	default:
		t.Fatalf("ScratchDatabase: no scratch database on dialect %s", dialect)
	}
	SetupFullSchema(t, db, dialect)
	return db
}

func createScratchPostgres(t *testing.T, name string) *sql.DB {
	t.Helper()
	base := PostgresTestDSN()
	configured := os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	admin, err := sql.Open("postgres", base)
	if err == nil {
		err = admin.Ping()
	}
	if err != nil {
		if admin != nil {
			admin.Close()
		}
		if configured {
			t.Fatalf("configured PostgreSQL at %s is unreachable: %v", redactDSN(base), err)
		}
		t.Skipf("no PostgreSQL at %s (default DSN, none configured): %v", redactDSN(base), err)
		return nil
	}
	t.Cleanup(func() { admin.Close() })

	// name is built from a prefix restricted to [a-z0-9_] and two integers, so there is nothing to
	// quote and nothing to inject.
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("creating the scratch database %s: %v (this needs a role with CREATEDB)", name, err)
	}
	dsn, err := swapDatabaseName(base, name)
	if err != nil {
		t.Fatalf("building a DSN for %s: %v", name, err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		// A new connection: the scratch database cannot be dropped while anyone is attached to it.
		a, err := sql.Open("postgres", base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	return db
}

func createScratchMSSQL(t *testing.T, name string) *sql.DB {
	t.Helper()
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
		return nil
	}
	admin, err := sql.Open("sqlserver", base)
	if err == nil {
		err = admin.Ping()
	}
	if err != nil {
		t.Fatalf("configured SQL Server at %s is unreachable: %v", redactDSN(base), err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec(`CREATE DATABASE [` + name + `]`); err != nil {
		t.Fatalf("creating the scratch database %s: %v", name, err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse the SQL Server DSN: %v", err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	t.Cleanup(func() {
		db.Close()
		a, err := sql.Open("sqlserver", base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(fmt.Sprintf(`IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END`, name))
	})
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	return db
}
