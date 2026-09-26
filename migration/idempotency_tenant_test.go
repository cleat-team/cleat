package migration_test

// package migration_test (external), not migration: see runner_test.go's
// file header for why -- engine/testutil now depends on this package.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/migration"
)

// 010_idempotency_keys_tenant_id.sql is a migration whose entire point is what
// it does to a database that already has rows in it, so testing it against a
// fresh schema would test the half that does not matter.
//
// IMPROVEMENT-PLAN 3.10 records two candidate fixes for the global
// idempotency key. Folding the tenant into the sha256 needs no migration and
// is one line per dialect -- and silently invalidates every key already
// stored, so the first retry after an upgrade starts a second workflow.
// Adding a tenant_id column keyed alongside key_hash keeps those rows
// matching, provided existing rows land on the tenant a single-tenant
// deployment already writes under. That proviso was the assertion below.
//
// TestIdempotencyTenantMigrationPreservesExistingKeys was removed by
// cleat#2434. It drove the runner over a staged directory holding
// 001_schema.sql alone, then 001_schema.sql plus
// 010_idempotency_keys_tenant_id.sql, and asserted that a key written before
// the upgrade still matched afterwards.
//
// The compaction folded 010 into MSSQL's three-file baseline, so there is no
// longer a file to stage and no longer a "before" state to build from the
// shipped tree -- which is what a frozen baseline means. After this PR no
// dialect has a partial chain left to stage. THAT is the argument, and it does
// not need any leg to have died quietly first.
//
// What the other two legs were actually doing, because this comment claimed the
// opposite until cleat#2438's review and the wrong version had already been
// copied into the PR body before it was caught:
//
//   * PostgreSQL SKIPS, and has since cleat#2059 -- that rebaseline left
//     postgres three files generated from a fully-migrated dump, so there is no
//     partial chain to build a before-state from. NOT because no DSN was
//     provided: the Test Go matrix runs ./migration/... and reaches PostgreSQL
//     through ci.yml's CLEAT_TEST_DB, which is the fallback that
//     CLEAT_TEST_POSTGRES is read with.
//   * MySQL RAN. multi-db-ci.yml sets CLEAT_TEST_MYSQL and runs
//     ./migration/... in the same job, and migrations/mysql/010_... exists --
//     that dialect was never rebaselined. Its arm was live until cleat#2435
//     added a skip to mysqlDialect(), which this PR's retirement makes dead
//     code.
//
// The distinction matters beyond this comment: "this was already gone" and
// "this PR is what ends it" are different claims about what the change costs.
//
// Reconstructing the pre-migration schema from git history was considered and
// rejected: CI checks out with fetch-depth 1, so the older revision is not
// there to read.
//
// The upgrade itself cannot regress -- a migration folded into a frozen
// baseline is never applied again, because every existing database already
// records its name in schema_migrations and every fresh one is built at the end
// state directly. What a baseline can get wrong is that end state, and
// scripts/gen-mssql-baseline -mode=diff and -mode=supplementary assert it
// catalogue-to-catalogue against a database built from the pre-compaction
// chain.
//
// The helpers below are shared with the other migration tests in this package
// and stay.

// idempotencyDialect carries the per-dialect knowledge the removed test needed,
// and the rest of the package still uses it: how to get an empty database, and
// the three places where the SQL genuinely differs. It is kept here rather than
// moved because that is where it has always lived and nothing about the
// compaction changes it.
type idempotencyDialect struct {
	dialect migration.Dialect
	// scratchDB returns a handle to an empty database, skipping the subtest
	// when this dialect is not configured.
	scratchDB func(t *testing.T) *sql.DB
	// rebind rewrites ? placeholders into the dialect's own form.
	rebind func(string) string
	// futureTimestamp is an expires_at expression comfortably in the future.
	futureTimestamp string
	// tenantIDText selects tenant_id as a string; SQL Server's
	// UNIQUEIDENTIFIER scans as a byte slice with reordered fields otherwise.
	tenantIDText string
	// skipReason, when set, skips this dialect with an explanation.
	skipReason string
}

// newMySQLScratchDB creates an empty MySQL database and returns a handle to it.
//
// Unlike the PostgreSQL helper this skips when CLEAT_TEST_MYSQL is unset,
// matching engine's MySQLBackend.Enabled: there is no default MySQL in CI's
// support matrix, so an unset variable means "not configured here" rather
// than "broken environment".
func newMySQLScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL migration test")
	}

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured MySQL is unreachable: %v", err)
	}
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop MySQL scratch database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create MySQL scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Logf("drop MySQL scratch database: %v", err)
		}
	})

	scratchDSN, err := swapMySQLDB(dsn, name)
	if err != nil {
		t.Fatalf("derive MySQL scratch DSN: %v", err)
	}
	db, err := sql.Open("mysql", scratchDSN)
	if err != nil {
		t.Fatalf("open MySQL scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL scratch database: %v", err)
	}
	return db
}

// swapMySQLDB replaces the database in a go-sql-driver DSN
// (user:pass@tcp(host:port)/dbname?params). Split on the last '/' before the
// query string rather than parsed as a URL, which the DSN is not.
func swapMySQLDB(dsn, name string) (string, error) {
	params := ""
	if i := lastIndexByte(dsn, '?'); i >= 0 {
		params = dsn[i:]
		dsn = dsn[:i]
	}
	i := lastIndexByte(dsn, '/')
	if i < 0 {
		return "", fmt.Errorf("no database component in MySQL DSN")
	}
	return dsn[:i+1] + name + params, nil
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// newMSSQLScratchDB creates an empty SQL Server database and returns a handle
// to it. Skips when CLEAT_TEST_MSSQL is unset, for the reason in
// newMySQLScratchDB.
func newMSSQLScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server migration test")
	}

	masterDSN, err := swapMSSQLDB(dsn, "master")
	if err != nil {
		t.Fatalf("derive SQL Server master DSN: %v", err)
	}
	admin, err := sql.Open("sqlserver", masterDSN)
	if err != nil {
		t.Fatalf("open SQL Server admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured SQL Server is unreachable: %v", err)
	}
	dropMSSQLScratchDB(t, admin, name)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create SQL Server scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("sqlserver", masterDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		dropMSSQLScratchDB(t, cleanup, name)
	})

	scratchDSN, err := swapMSSQLDB(dsn, name)
	if err != nil {
		t.Fatalf("derive SQL Server scratch DSN: %v", err)
	}
	db, err := sql.Open("sqlserver", scratchDSN)
	if err != nil {
		t.Fatalf("open SQL Server scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping SQL Server scratch database: %v", err)
	}
	return db
}

// dropMSSQLScratchDB drops the scratch database, evicting whatever a killed
// previous run left connected: SQL Server refuses to drop a database that has
// sessions on it, and SINGLE_USER WITH ROLLBACK IMMEDIATE is how you take them
// off it.
func dropMSSQLScratchDB(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	_, _ = admin.Exec(`IF DB_ID('` + name + `') IS NOT NULL
		ALTER DATABASE ` + name + ` SET SINGLE_USER WITH ROLLBACK IMMEDIATE`)
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Logf("drop SQL Server scratch database: %v", err)
	}
}

// swapMSSQLDB replaces the database query parameter in a sqlserver:// URL.
func swapMSSQLDB(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
