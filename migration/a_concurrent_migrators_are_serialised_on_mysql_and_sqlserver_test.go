package migration_test

// Two migrators started at once on the same database (cleat#2117).
//
// Migration is now a deploy step (cleat-worker --migrate-only), so the case that
// matters is a deploy job and a straggling worker, or two deploy jobs, migrating
// the same database together. PostgreSQL has always serialised that with an
// advisory lock (TestRunner_ConcurrentRunsAreSerialised). MySQL and SQL Server did
// not, and this comment used to say that was deliberate.
//
// MEASURED before the lock existed, four concurrent Runs against an EMPTY database
// (2026-09-23, this test with the locking removed):
//
//	PostgreSQL   4 of 4 succeed
//	MySQL        1 of 4 succeed; three fail: Error 1061 "Duplicate key name
//	             'idx_instances_ready'" from 001_schema.sql
//	SQL Server   1 of 4 succeed; three fail: 1205 deadlock victim, 2021 "the
//	             referenced entity was modified during DDL execution", 2714
//	             "already an object named 'finalize_workflow_status'"
//
// Every failure is a process that exits at boot. The tracking table stayed
// consistent in both, which is why this asserts on the errors and not only on the
// end state: an end-state check would have passed a run in which three of four
// migrators had died.
//
// The tests skip when their DSN is unset, like every other database test here. CI's
// multi-database jobs set all three; see CLAUDE.md, "Is this result real?".

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/migration"
	"github.com/go-sql-driver/mysql"
)

type otherDialect struct {
	env    string
	driver string
	d      migration.Dialect
}

// scratchDSN creates an EMPTY database on the server the dialect's test DSN names
// and returns a DSN for it. The name is unique per call.
func scratchDSN(t *testing.T, c otherDialect, name string) string {
	t.Helper()
	admin := os.Getenv(c.env)
	adb, err := sql.Open(c.driver, admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adb.Close()
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", c.env, err)
	}

	var scratch string
	switch c.d {
	case migration.DialectMySQL:
		cfg, err := mysql.ParseDSN(admin)
		if err != nil {
			t.Fatalf("parse %s: %v", c.env, err)
		}
		if _, err := adb.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
			t.Fatalf("create scratch database: %v", err)
		}
		cfg.DBName = name
		scratch = cfg.FormatDSN()
		t.Cleanup(func() {
			if a, err := sql.Open(c.driver, admin); err == nil {
				defer a.Close()
				_, _ = a.Exec("DROP DATABASE IF EXISTS `" + name + "`")
			}
		})
	case migration.DialectMSSQL:
		u, err := url.Parse(admin)
		if err != nil {
			t.Fatalf("parse %s: %v", c.env, err)
		}
		if _, err := adb.Exec(fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if _, err := adb.Exec("CREATE DATABASE [" + name + "]"); err != nil {
			t.Fatalf("create scratch database: %v", err)
		}
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
		scratch = u.String()
		t.Cleanup(func() {
			if a, err := sql.Open(c.driver, admin); err == nil {
				defer a.Close()
				_, _ = a.Exec(fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name))
			}
		})
	}
	return scratch
}

// shippedMigrationCount counts the NNN_*.sql files for a dialect: the number of
// rows a fully migrated schema_migrations must hold.
func shippedMigrationCount(t *testing.T, dialect string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsRoot(t), dialect, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	rx := regexp.MustCompile(`^\d+_`)
	for _, f := range files {
		if rx.MatchString(filepath.Base(f)) {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no migrations found for %s", dialect)
	}
	return n
}

func TestConcurrentMigratorsAreSerialisedOnMySQLAndSQLServer(t *testing.T) {
	for _, c := range []otherDialect{
		{"CLEAT_TEST_MYSQL", "mysql", migration.DialectMySQL},
		{"CLEAT_TEST_MSSQL", "sqlserver", migration.DialectMSSQL},
	} {
		t.Run(string(c.d), func(t *testing.T) {
			if os.Getenv(c.env) == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.d)
			}
			dsn := scratchDSN(t, c, fmt.Sprintf("cleat_2117_concurrent_%d", time.Now().UnixNano()%1_000_000_000))
			root := migrationsRoot(t)
			want := shippedMigrationCount(t, string(c.d))

			const runners = 4
			var wg sync.WaitGroup
			errs := make([]error, runners)
			start := make(chan struct{})
			for i := 0; i < runners; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					// Its own pool each, as separate processes would have.
					db, err := sql.Open(c.driver, dsn)
					if err != nil {
						errs[i] = err
						return
					}
					defer db.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					<-start
					errs[i] = runMigrations(t, ctx, migration.NewRunner(db, c.d, root), c.d)
				}(i)
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent migrator %d failed: %v", i, err)
				}
			}

			db, err := sql.Open(c.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			count := func() (rows, distinct int) {
				if err := db.QueryRow(`SELECT count(*), count(DISTINCT version) FROM schema_migrations`).Scan(&rows, &distinct); err != nil {
					t.Fatalf("count schema_migrations: %v", err)
				}
				return
			}
			rows, distinct := count()
			if rows != want || distinct != want {
				t.Errorf("schema_migrations has %d rows (%d distinct), want %d: %s migrations shipped",
					rows, distinct, want, c.d)
			}

			// A second run is a no-op: it succeeds and changes nothing.
			if err := runMigrations(t, context.Background(), migration.NewRunner(db, c.d, root), c.d); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if r2, d2 := count(); r2 != rows || d2 != distinct {
				t.Errorf("a second run changed schema_migrations: %d/%d -> %d/%d", rows, distinct, r2, d2)
			}

			// The lock was released. On MySQL GET_LOCK is session-scoped and a pooled
			// connection would carry a leaked one, so ask the server directly.
			if c.d == migration.DialectMySQL {
				var holder sql.NullInt64
				if err := db.QueryRow(`SELECT IS_USED_LOCK(CONCAT('cleat.migrations.', MD5(DATABASE())))`).Scan(&holder); err != nil {
					t.Fatal(err)
				}
				if holder.Valid {
					t.Errorf("the migration lock is still held by connection %d after every run finished", holder.Int64)
				}
			}
		})
	}
}
