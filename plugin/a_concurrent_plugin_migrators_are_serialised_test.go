package plugin

// Plugin migrations, four at once on MySQL and SQL Server (cleat#2117).
//
// The plugin runner is the SECOND migration runner (see RunMigrations), and it had
// the same gap as the core one: PostgreSQL took an advisory lock, MySQL and SQL
// Server took nothing. Migration is now a deploy step run alongside straggling
// workers, so two runs at once is the ordinary case. The core runner's test
// (migration/a_concurrent_migrators_are_serialised_*) carries the measurement of
// what happened without the lock; this is the same question asked of this file.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"
)

func TestConcurrentPluginMigratorsAreSerialisedOnMySQLAndSQLServer(t *testing.T) {
	for _, c := range []struct {
		env, driver string
		d           Dialect
	}{
		{"CLEAT_TEST_MYSQL", "mysql", DialectMySQL},
		{"CLEAT_TEST_MSSQL", "sqlserver", DialectMSSQL},
	} {
		t.Run(string(c.d), func(t *testing.T) {
			admin := os.Getenv(c.env)
			if admin == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.d)
			}
			name := fmt.Sprintf("cleat_2117_plugins_%d", time.Now().UnixNano()%1_000_000_000)
			adb, err := sql.Open(c.driver, admin)
			if err != nil {
				t.Fatal(err)
			}
			defer adb.Close()
			if err := adb.Ping(); err != nil {
				t.Fatalf("%s is set but unreachable: %v", c.env, err)
			}
			var dsn string
			switch c.d {
			case DialectMySQL:
				cfg, err := mysql.ParseDSN(admin)
				if err != nil {
					t.Fatal(err)
				}
				mustExec(t, adb, "DROP DATABASE IF EXISTS `"+name+"`")
				mustExec(t, adb, "CREATE DATABASE `"+name+"`")
				t.Cleanup(func() { _, _ = adb.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
				cfg.DBName = name
				dsn = cfg.FormatDSN()
			default:
				u, err := url.Parse(admin)
				if err != nil {
					t.Fatal(err)
				}
				drop := fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)
				mustExec(t, adb, drop)
				mustExec(t, adb, "CREATE DATABASE ["+name+"]")
				t.Cleanup(func() { _, _ = adb.Exec(drop) })
				q := u.Query()
				q.Set("database", name)
				u.RawQuery = q.Encode()
				dsn = u.String()
			}

			p := &testMigrationPlugin{
				info: PluginInfo{Name: "cleat-2117-probe"},
				migrations: []Migration{{
					Version: 1,
					Up:      `CREATE TABLE cleat_2117_probe (id int PRIMARY KEY)`,
					UpMySQL: `CREATE TABLE cleat_2117_probe (id int PRIMARY KEY)`,
					UpMSSQL: `CREATE TABLE cleat_2117_probe (id int PRIMARY KEY)`,
				}},
			}
			lp := &LoadedPlugin{Plugin: p, Healthy: true}

			const runners = 4
			var wg sync.WaitGroup
			errs := make([]error, runners)
			start := make(chan struct{})
			for i := 0; i < runners; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					db, err := sql.Open(c.driver, dsn)
					if err != nil {
						errs[i] = err
						return
					}
					defer db.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					<-start
					errs[i] = RunMigrations(ctx, db, c.d, nil, []*LoadedPlugin{lp})
				}(i)
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent plugin migrator %d failed: %v", i, err)
				}
			}

			db, err := sql.Open(c.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var n int
			q := `SELECT count(*) FROM plugin_migrations WHERE plugin_name = 'cleat-2117-probe'`
			if err := db.QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("count plugin_migrations: %v", err)
			}
			if n != 1 {
				t.Errorf("plugin_migrations holds %d rows for the probe plugin, want exactly 1", n)
			}
			if c.d == DialectMySQL {
				var holder sql.NullInt64
				if err := db.QueryRow(`SELECT IS_USED_LOCK(CONCAT('cleat.plugin_migrations.', MD5(DATABASE())))`).Scan(&holder); err != nil {
					t.Fatal(err)
				}
				if holder.Valid {
					t.Errorf("the plugin migration lock is still held by connection %d", holder.Int64)
				}
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
