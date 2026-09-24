package main

// Migration is a deploy step (cleat#2117): `cleat-worker --migrate-only` applies the
// schema and exits, and a normal start VERIFIES it and refuses, with the remediation,
// if it is behind. This runs the real binary against an empty database on each
// dialect, because every property here is about what the PROCESS does -- its exit
// status, what it printed, and what it left in the database -- and a unit test of the
// helpers would pass with the flags wired to nothing.
//
// The rule for a schema AHEAD of the binary is tested as well, and on purpose: a
// rolling upgrade exercises exactly that path (the deploy job migrates to N+1 while
// workers on N are still restarting), and a worker that refused an ahead schema would
// wedge the rollout on the workers it is trying to replace.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
)

type deployDialect struct {
	name   string // the worker's --driver
	env    string
	driver string // database/sql driver name
}

// deployScratch creates an EMPTY database for one dialect and returns the DSN the
// worker should use and a handle for inspecting it.
func deployScratch(t *testing.T, c deployDialect) (dsn string, db *sql.DB) {
	t.Helper()
	admin := os.Getenv(c.env)
	adb, err := sql.Open(c.driver, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", c.env, err)
	}
	name := fmt.Sprintf("cleat_2117_deploy_%d", time.Now().UnixNano()%1_000_000_000)
	switch c.name {
	case "postgres":
		if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			a, err := sql.Open(c.driver, admin)
			if err != nil {
				return
			}
			defer a.Close()
			_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
			_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
		})
		u, _ := url.Parse(admin)
		u.Path = "/" + name
		dsn = u.String()
	case "mysql":
		cfg, err := mysql.ParseDSN(admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			a, err := sql.Open(c.driver, admin)
			if err != nil {
				return
			}
			defer a.Close()
			_, _ = a.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		})
		cfg.DBName = name
		dsn = cfg.FormatDSN()
	default:
		drop := fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)
		if _, err := adb.Exec("CREATE DATABASE [" + name + "]"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			a, err := sql.Open(c.driver, admin)
			if err != nil {
				return
			}
			defer a.Close()
			_, _ = a.Exec(drop)
		})
		u, _ := url.Parse(admin)
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	db, err = sql.Open(c.driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return dsn, db
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runWorker runs the binary to completion (for the modes that exit) and returns its
// exit code and combined output.
func runWorker(t *testing.T, bin string, env []string, args ...string) (int, string) {
	t.Helper()
	const bound = 2 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("`cleat-worker %s` did not exit within %s: a mode that should refuse or finish kept "+
			"running, which means it started serving.\n%s", strings.Join(args, " "), bound, out)
	}
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run %s %v: %v", bin, args, err)
	return -1, ""
}

// startsHealthy runs the worker as a server and reports whether it reached
// /healthz within the time allowed, killing it afterwards. Its output is returned
// either way, for the assertions that read a warning out of it.
func startsHealthy(t *testing.T, bin string, args ...string) (bool, string) {
	t.Helper()
	port := freePort(t)
	cmd := exec.Command(bin, append(args, fmt.Sprintf("--api-addr=127.0.0.1:%d", port), "--require-auth=false")...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return false, out.String()
		default:
		}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true, out.String()
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false, out.String()
}

func TestMigrationIsADeployStepOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	for _, c := range []deployDialect{
		{"postgres", "CLEAT_TEST_POSTGRES", "postgres"},
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if os.Getenv(c.env) == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			dsn, db := deployScratch(t, c)
			base := []string{"--driver=" + c.name, "--db=" + dsn}
			count := func(q string) int {
				var n int
				if err := db.QueryRow(q).Scan(&n); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return n
			}
			migrations := func() int { return count(`SELECT count(*) FROM schema_migrations`) }

			// 1. THE KNOWN-POSITIVE from the issue: a worker started on an un-migrated
			//    database REFUSES, and the message is the remediation.
			code, out := runWorker(t, bin, nil, base...)
			if code == 0 {
				t.Fatalf("a worker on an un-migrated database exited 0; it must refuse.\n%s", out)
			}
			// The CORE message specifically: the plugin check also refuses an
			// un-migrated database, and asserting only on "schema is behind" let a
			// core check that had been switched off pass on the plugin layer's answer.
			for _, want := range []string{"the database schema is behind this worker", "never been migrated", "--migrate-only"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal must carry the remediation; missing %q in:\n%s", want, out)
				}
			}
			// ... and refusing changed NOTHING: verify is read-only.
			if n := tableCount(t, db, c.name, "schema_migrations"); n != 0 {
				t.Errorf("a refused start created schema_migrations; verify must not write")
			}

			// 2. --migrate-only brings it to the current version and exits 0.
			code, out = runWorker(t, bin, nil, append(base, "--migrate-only")...)
			if code != 0 {
				t.Fatalf("--migrate-only exited %d:\n%s", code, out)
			}
			first := migrations()
			if first == 0 {
				t.Fatal("--migrate-only exited 0 but applied nothing")
			}

			// 3. Idempotent: the second run is a no-op.
			code, out = runWorker(t, bin, nil, append(base, "--migrate-only")...)
			if code != 0 {
				t.Fatalf("second --migrate-only exited %d:\n%s", code, out)
			}
			if second := migrations(); second != first {
				t.Errorf("a second --migrate-only changed schema_migrations: %d -> %d", first, second)
			}

			// 4. EQUAL: a normal start on the migrated database starts.
			if ok, out := startsHealthy(t, bin, base...); !ok {
				t.Fatalf("a worker on a fully migrated database did not start:\n%s", out)
			}

			// 5. AHEAD: a version the binary does not ship. It STARTS, with a warning
			//    naming both versions -- the rolling-upgrade path.
			ahead := 999999
			if _, err := db.Exec(insertVersionSQL(c.name), fmt.Sprint(ahead)); err != nil {
				t.Fatalf("seed an ahead version: %v", err)
			}
			ok, out := startsHealthy(t, bin, base...)
			if !ok {
				t.Fatalf("a worker refused a schema AHEAD of it; a rolling upgrade would wedge:\n%s", out)
			}
			for _, want := range []string{"AHEAD", "999999"} {
				if !strings.Contains(out, want) {
					t.Errorf("the ahead warning should name the versions; missing %q in:\n%s", want, out)
				}
			}
			if _, err := db.Exec(deleteVersionSQL(c.name), fmt.Sprint(ahead)); err != nil {
				t.Fatal(err)
			}

			// 6. BEHIND by one: the newest applied migration is removed from the
			//    tracking table. It refuses, and names it.
			var newest string
			if err := db.QueryRow(newestVersionSQL(c.name)).Scan(&newest); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(deleteVersionSQL(c.name), newest); err != nil {
				t.Fatal(err)
			}
			code, out = runWorker(t, bin, nil, base...)
			if code == 0 {
				t.Fatalf("a worker one migration behind exited 0:\n%s", out)
			}
			if !strings.Contains(out, "the database schema is behind this worker") ||
				!strings.Contains(out, "1 of ") || !strings.Contains(out, "not applied") {
				t.Errorf("the refusal should be the CORE check saying one migration is missing:\n%s", out)
			}
			// Put it back for the rest: --migrate-only repairs it.
			if code, out = runWorker(t, bin, nil, append(base, "--migrate-only")...); code != 0 {
				t.Fatalf("--migrate-only did not repair a missing tracking row: %d\n%s", code, out)
			}
			if got := migrations(); got != first {
				t.Errorf("after repair schema_migrations has %d rows, want %d", got, first)
			}

			// 6b. A PLUGIN migration behind: same rule, its own message. Last of the
			//     destructive steps because re-running a plugin migration whose row was
			//     deleted would try to CREATE a table that exists.
			//     (The row is restored by hand below, so step 7 can migrate again.)
			var pluginName string
			var pluginVersion int
			if err := db.QueryRow(firstPluginMigrationSQL(c.name)).Scan(&pluginName, &pluginVersion); err != nil {
				t.Fatalf("read a plugin migration row: %v", err)
			}
			if _, err := db.Exec(deletePluginMigrationSQL(c.name), pluginName, pluginVersion); err != nil {
				t.Fatal(err)
			}
			code, out = runWorker(t, bin, nil, base...)
			if code == 0 {
				t.Fatalf("a worker with a plugin migration behind exited 0:\n%s", out)
			}
			if !strings.Contains(out, "plugin schema is behind") || !strings.Contains(out, pluginName) {
				t.Errorf("the refusal should be the PLUGIN check naming %s:\n%s", pluginName, out)
			}
			if _, err := db.Exec(insertPluginMigrationRowSQL(c.name), pluginName, pluginVersion); err != nil {
				t.Fatalf("restore the plugin migration row: %v", err)
			}

			// 7. --migrate-only needs NO key ring and never reaches the secrets check.
			//    A stored secret and no CLEAT_SECRET_MASTER_KEY is exactly the case in
			//    which a normal start refuses; a deploy job has no reason to hold the
			//    key, so it must not.
			if _, err := db.Exec(insertSecretSQL(c.name)); err != nil {
				t.Fatalf("seed a stored secret: %v", err)
			}
			if code, out = runWorker(t, bin, []string{"CLEAT_SECRET_MASTER_KEY="}, append(base, "--migrate-only")...); code != 0 {
				t.Fatalf("--migrate-only refused because of a stored secret it has no key for (%d): it reached the secrets check.\n%s", code, out)
			}
		})
	}
}

func tableCount(t *testing.T, db *sql.DB, dialect, table string) int {
	t.Helper()
	var q string
	switch dialect {
	case "mysql":
		q = `SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '` + table + `'`
	case "mssql":
		q = `SELECT count(*) FROM sys.tables WHERE name = '` + table + `'`
	default:
		q = `SELECT count(*) FROM information_schema.tables WHERE table_name = '` + table + `' AND table_schema NOT IN ('pg_catalog','information_schema')`
	}
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func insertVersionSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO schema_migrations (version, applied_at) VALUES (?, NOW(6))`
	case "mssql":
		return `INSERT INTO schema_migrations (version, applied_at) VALUES (@p1, SYSUTCDATETIME())`
	}
	return `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())`
}

func deleteVersionSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return `DELETE FROM schema_migrations WHERE version = ?`
	case "mssql":
		return `DELETE FROM schema_migrations WHERE version = @p1`
	}
	return `DELETE FROM schema_migrations WHERE version = $1`
}

// newestVersionSQL is the highest NUMERIC version: the column is text, so ORDER BY
// version would put '99' after '100'.
func newestVersionSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT version FROM schema_migrations ORDER BY CAST(version AS UNSIGNED) DESC LIMIT 1`
	case "mssql":
		return `SELECT TOP 1 version FROM schema_migrations ORDER BY CAST(version AS int) DESC`
	}
	return `SELECT version FROM schema_migrations ORDER BY version::int DESC LIMIT 1`
}

// insertSecretSQL stores one secret row under the default tenant, sealed under
// nothing the worker could open. It is only ever counted, never decrypted.
func insertSecretSQL(dialect string) string {
	const tenant = "00000000-0000-0000-0000-000000000000"
	_ = dialect
	return `INSERT INTO tenant_secrets (tenant_id, name, ciphertext, key_version) VALUES ('` + tenant + `', 'cleat-2117', 'x', 1)`
}

func firstPluginMigrationSQL(dialect string) string {
	if dialect == "mssql" {
		return `SELECT TOP 1 plugin_name, version FROM plugin_migrations ORDER BY plugin_name, version`
	}
	return `SELECT plugin_name, version FROM plugin_migrations ORDER BY plugin_name, version LIMIT 1`
}

func deletePluginMigrationSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return `DELETE FROM plugin_migrations WHERE plugin_name = ? AND version = ?`
	case "mssql":
		return `DELETE FROM plugin_migrations WHERE plugin_name = @p1 AND version = @p2`
	}
	return `DELETE FROM plugin_migrations WHERE plugin_name = $1 AND version = $2`
}

func insertPluginMigrationRowSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES (?, ?)`
	case "mssql":
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES (@p1, @p2)`
	}
	return `INSERT INTO plugin_migrations (plugin_name, version) VALUES ($1, $2)`
}
