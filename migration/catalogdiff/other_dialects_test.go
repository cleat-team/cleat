package catalogdiff

// cleat#2059's compaction spans all three dialects (migrations/postgres,
// migrations/mysql, migrations/mssql), so this harness cannot ship verified
// on PostgreSQL alone -- the same identity check catalogdiff_test.go proves
// for PostgreSQL has to hold on MySQL and SQL Server too, or a MySQL/MSSQL
// compaction would be "verified" by a tool that has never been run against
// either. The two mandated known-positives (wrong routine body, dropped
// FORCE) are PostgreSQL-specific -- FORCE ROW LEVEL SECURITY has no MySQL or
// SQL Server equivalent in this schema -- so only the identity check and the
// plugin-object precondition are dialect-general here.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/migration"
)

func mysqlAdminDSN() string { return os.Getenv("CLEAT_TEST_MYSQL") }
func mssqlAdminDSN() string { return os.Getenv("CLEAT_TEST_MSSQL") }

// scratchMySQLDB creates a fresh, uniquely-named, empty MySQL database,
// applies the full current migrations/mysql/ chain via migration.NewRunner,
// and returns a handle. Dropped in t.Cleanup.
func scratchMySQLDB(t *testing.T) *sql.DB {
	t.Helper()
	admin := mysqlAdminDSN()
	adb, err := sql.Open("mysql", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MYSQL is set but unreachable: %v", err)
	}

	name := fmt.Sprintf("cleat_2059_catalogdiff_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatalf("creating scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS `" + name + "`")
	})

	cfg, err := mysql.ParseDSN(admin)
	if err != nil {
		t.Fatalf("parsing admin DSN: %v", err)
	}
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to freshly created %s: %v", name, err)
	}

	ctx := context.Background()
	if err := migration.NewRunner(db, migration.DialectMySQL, postgresMigrationsDir(t)).Run(ctx); err != nil {
		t.Fatalf("applying migrations/mysql/ to scratch database %s: %v", name, err)
	}
	return db
}

// scratchMSSQLDB creates a fresh, uniquely-named, empty SQL Server database,
// applies the full current migrations/mssql/ chain via migration.NewRunner,
// and returns a handle. Dropped in t.Cleanup.
func scratchMSSQLDB(t *testing.T) *sql.DB {
	t.Helper()
	admin := mssqlAdminDSN()
	adb, err := sql.Open("sqlserver", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but unreachable: %v", err)
	}

	name := fmt.Sprintf("cleat_2059_catalogdiff_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec("CREATE DATABASE [" + name + "]"); err != nil {
		t.Fatalf("creating scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("sqlserver", admin)
		if err != nil {
			return
		}
		defer a.Close()
		drop := fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)
		_, _ = a.Exec(drop)
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parsing admin DSN: %v", err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to freshly created %s: %v", name, err)
	}

	ctx := context.Background()
	if err := migration.NewRunner(db, migration.DialectMSSQL, postgresMigrationsDir(t)).Run(ctx); err != nil {
		t.Fatalf("applying migrations/mssql/ to scratch database %s: %v", name, err)
	}
	return db
}

func TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMySQL(t *testing.T) {
	if mysqlAdminDSN() == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping")
	}
	a, err := Snapshot(context.Background(), scratchMySQLDB(t), migration.DialectMySQL)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	b, err := Snapshot(context.Background(), scratchMySQLDB(t), migration.DialectMySQL)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(a.Tables) == 0 {
		t.Fatal("scratch database A has zero tables after applying the full migration chain")
	}
	if diff := Diff(a, b); len(diff) != 0 {
		t.Fatalf("two independently-built MySQL databases from the identical migration chain differ (%d lines):\n%s",
			len(diff), joinLines(diff))
	}
}

func TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMSSQL(t *testing.T) {
	if mssqlAdminDSN() == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping")
	}
	a, err := Snapshot(context.Background(), scratchMSSQLDB(t), migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	b, err := Snapshot(context.Background(), scratchMSSQLDB(t), migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(a.Tables) == 0 {
		t.Fatal("scratch database A has zero tables after applying the full migration chain")
	}
	if diff := Diff(a, b); len(diff) != 0 {
		t.Fatalf("two independently-built SQL Server databases from the identical migration chain differ (%d lines):\n%s",
			len(diff), joinLines(diff))
	}
}

func TestNeitherScratchDatabaseContainsAPluginObjectMySQL(t *testing.T) {
	if mysqlAdminDSN() == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping")
	}
	db := scratchMySQLDB(t)
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectMySQL); err != nil {
		t.Fatalf("a scratch database built by migration.NewRunner alone should never contain a plugin object: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE plugin_migrations (plugin_name TEXT)`); err != nil {
		t.Fatalf("creating a fake plugin_migrations table: %v", err)
	}
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectMySQL); err == nil {
		t.Fatal("AssertNoPluginObjects returned nil after plugin_migrations was created")
	}
}

func TestNeitherScratchDatabaseContainsAPluginObjectMSSQL(t *testing.T) {
	if mssqlAdminDSN() == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping")
	}
	db := scratchMSSQLDB(t)
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectMSSQL); err != nil {
		t.Fatalf("a scratch database built by migration.NewRunner alone should never contain a plugin object: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE plugin_migrations (plugin_name VARCHAR(200))`); err != nil {
		t.Fatalf("creating a fake plugin_migrations table: %v", err)
	}
	if err := AssertNoPluginObjects(context.Background(), db, migration.DialectMSSQL); err == nil {
		t.Fatal("AssertNoPluginObjects returned nil after plugin_migrations was created")
	}
}
