package main

// cleat#2203. PostgreSQL's cleat_app has had SELECT-only access to deployment_secrets
// since the baseline (migrations/postgres/001_schema.sql); MySQL and SQL Server had no
// application login at all, so the one login every worker used could write it too
// (docs/how-to/use-deployment-secrets.md used to say exactly that -- "no equivalent role
// split to revoke from"). This adds cleat_app to both, following three rounds of
// cleat-review measurement against real databases:
//
//   - migrations/mssql/007_app_login.sql creates a database ROLE (cleat_app_role)
//     carrying the GRANT/DENY; deploy/mssql/900-app-role.sh creates the LOGIN and adds
//     it to that role. Splitting it this way is required, not stylistic: CREATE LOGIN
//     needs securityadmin/sysadmin, which a SQL Server migrate login has never needed
//     before (G2) -- measured by running 007 as a plain db_owner login with no server
//     role, which develop itself migrates cleanly and which failed on an unsplit
//     version of this file with "User does not have permission to perform this action.
//     (15247)".
//   - MySQL has no database-scoped principal at all, so CREATE USER and every GRANT
//     move out of migrations/ entirely into deploy/mysql/900-app-role.sh (G2: CREATE
//     USER needs the global CREATE USER privilege, and GRANT needs GRANT OPTION on
//     each privilege granted, neither of which a MySQL migrate login has ever needed).
//   - Both scripts set a real, unknowable password in the SAME action that creates the
//     login, rather than a dormant locked/disabled account a later "just enable it"
//     step could open with an empty or published password (G3).
//   - deploy/mysql/900-app-role.sh grants cleat_app full rights on `cleat\_%`, not just
//     the one named database: MySQL isolates tenants with one database per tenant
//     (engine.MySQLTenantDatabaseName), created and migrated lazily through the SAME
//     connection the worker serves on, so cleat_app needs DDL there too or a worker
//     cannot even boot (G1) -- measured directly: TestAWorkerBootsAndServesAsTheAppLoginOnEveryDialect's
//     mysql subtest failed at "create tenant database: Access denied" before this
//     grant existed, and a hardcoded list of the 29 core tables alone was not enough
//     either -- a booted worker with its default plugin set failed on `rate_limits`,
//     `oauth_sessions`, `tenant_trials`, `task_queue`, `schedules`, `backup_config` and
//     `backup_history`, all created by PLUGIN migrations the hardcoded list had never
//     heard of. The script queries information_schema instead, for the same reason
//     this file's own mysqlAppRoleDSN below does.
//
// THIS FILE'S mysqlAppRoleDSN/mssqlAppRoleDSN DO NOT EXECUTE THE DEPLOY SCRIPTS. Doing
// so would need the `mysql` and `sqlcmd` client binaries on whatever machine runs `go
// test` -- confirmed absent from this repo's own CI (multi-db-ci.yml only ever reaches
// sqlcmd through `docker exec` into the SQL Server container itself, and never installs
// a `mysql` client at all) and from the development machine this PR was written on.
// `deploy/postgres/900-app-role.sh` has exactly the same shape and is untested by any
// Go test for the same reason (`psql` is equally absent here). So these two functions
// are a SEPARATE implementation of the scripts' SQL, in Go, over the same `database/sql`
// connection every other helper in this file already uses -- matching, not calling, the
// shipped artifact. Keeping the two in sync is manual; a shared Go implementation behind
// a cleatctl subcommand, callable from both, would remove that risk and is follow-up
// work beyond this PR's three required fixes.
//
// Each dialect's write-restriction assertion has its OWN shape, not a shared helper
// pretending the two privilege models are the same thing: MySQL has no DENY, so the
// proof is "no grant for write was ever issued"; SQL Server's DENY overrides a broader
// GRANT regardless of order, so the proof is the DENY actually refusing an operation its
// schema-level GRANT would otherwise allow. Postgres is included as a known-positive: if
// the harness (deployScratch, pgAppRoleDSN, runWorker) cannot see PostgreSQL's existing,
// already shipped restriction, it cannot be trusted to see the two new ones either.
//
// Each case also proves two things a passing INSERT refusal alone would not:
//   - the SAME login can still write an ordinary table (otherwise "denied" might mean
//     "this login cannot write anything", which is not what shipped)
//   - the owner/migrate login CAN still write deployment_secrets (otherwise the
//     cleatctl set-deployment-secret / retire-deployment-secret path this leaves
//     untouched would itself be broken, and a reader could not tell from this test)

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// randomTestPassword generates a password for a scratch login this test creates and
// drops, per cleat-review's N2b: a hardcoded literal here is indistinguishable, to
// gitleaks, from a real credential, and was flagged as one. The hex body contains
// neither a quote nor a backslash, so it is safe to splice directly into both a MySQL
// single-quoted string (fmt.Sprintf's '%s') and a T-SQL one (string concatenation) with
// no escaping. The suffix exists only to satisfy SQL Server's default password
// complexity policy (upper, lower, digit) when it is enabled on the test container.
func randomTestPassword(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating a random test password: %v", err)
	}
	return hex.EncodeToString(buf) + "Aa1!"
}

func TestTheAppLoginCannotWriteDeploymentSecrets(t *testing.T) {
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
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			ownerDSN, owner := deployScratch(t, c)
			if code, out := runWorker(t, bin, nil, "--driver="+c.name, "--db="+ownerDSN, "--migrate-only"); code != 0 {
				t.Fatalf("--migrate-only exited %d:\n%s", code, out)
			}

			var appDSN string
			switch c.name {
			case "postgres":
				appDSN = pgAppRoleDSN(t, owner, ownerDSN)
			case "mysql":
				appDSN = mysqlAppRoleDSN(t, owner, ownerDSN, false)
			case "mssql":
				appDSN = mssqlAppRoleDSN(t, owner, ownerDSN)
			}
			app, err := sql.Open(c.driver, appDSN)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.Close() })

			insertSecret, writeProbe := deploymentSecretsInsert(c.name), writeProbeStmt(c.name)

			// KNOWN-POSITIVE, as the owner/migrate login: both statements must succeed on a
			// freshly migrated database, or a refusal below proves nothing about the app
			// login specifically -- it could just as well be a malformed statement or a
			// table that does not exist yet.
			if _, err := owner.Exec(insertSecret, "known-positive", "ct"); err != nil {
				t.Fatalf("the owner login could not insert into deployment_secrets on a fresh database "+
					"(is the schema baseline applied?): %v", err)
			}
			if _, err := owner.Exec(writeProbe, "cleat_2203_probe_owner"); err != nil {
				t.Fatalf("the owner login could not write the ordinary table this test also uses as a "+
					"negative control: %v", err)
			}

			// THE REFUSAL. The app login must not be able to write deployment_secrets.
			if _, err := app.Exec(insertSecret, "should-be-refused", "ct"); err == nil {
				t.Fatal("the app login inserted into deployment_secrets; the write restriction cleat#2203 adds is not in effect")
			}

			// READ STILL WORKS. The restriction is write-only -- see
			// docs/how-to/use-deployment-secrets.md's "read it back" note: a worker still
			// needs to GET a deployment secret at call time.
			var n int
			if err := app.QueryRow(`SELECT count(*) FROM deployment_secrets`).Scan(&n); err != nil {
				t.Fatalf("the app login could not SELECT from deployment_secrets, which it needs to "+
					"resolve a secret at runtime: %v", err)
			}
			if n < 1 {
				t.Fatalf("expected at least the known-positive row inserted above, got %d", n)
			}

			// NEGATIVE CONTROL. The app login is not broadly broken -- it can still write the
			// tables a worker actually serves on.
			if _, err := app.Exec(writeProbe, "cleat_2203_probe_app"); err != nil {
				t.Fatalf("the app login could not write an ordinary table it needs for normal operation "+
					"(cleat#2203's grant is too narrow): %v", err)
			}
		})
	}
}

// TestAWorkerBootsAndServesAsTheAppLoginOnEveryDialect is cleat-review's G1 probe, kept
// as a permanent regression test rather than a one-off measurement: the worker must
// actually BOOT and SERVE a request as the app login, not merely pass individual SQL
// statements run directly against the database. MySQL's failure mode was exactly this
// gap -- TestTheAppLoginCannotWriteDeploymentSecrets's mysql subtest, which only ever
// opens its own ad hoc *sql.DB against the already-migrated scratch database, could not
// have caught "the worker cannot create its OWN tenant database at boot", because that
// step runs during `--migrate-only`/boot, before this test's app connection exists.
func TestAWorkerBootsAndServesAsTheAppLoginOnEveryDialect(t *testing.T) {
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
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			ownerDSN, owner := deployScratch(t, c)
			if code, out := runWorker(t, bin, nil, "--driver="+c.name, "--db="+ownerDSN, "--migrate-only"); code != 0 {
				t.Fatalf("--migrate-only exited %d:\n%s", code, out)
			}

			var appDSN string
			switch c.name {
			case "postgres":
				appDSN = pgAppRoleDSN(t, owner, ownerDSN)
			case "mysql":
				appDSN = mysqlAppRoleDSN(t, owner, ownerDSN, true)
			case "mssql":
				appDSN = mssqlAppRoleDSN(t, owner, ownerDSN)
			}

			args := []string{
				"--driver=" + c.name, "--db=" + appDSN, "--migrate-db=" + ownerDSN,
				"--require-auth=false",
			}
			ok, out := startsHealthy(t, bin, args...)
			if !ok {
				t.Fatalf("the worker did not become healthy serving as the app login:\n%s", out)
			}
			// A worker that starts and answers /healthz can still have failed to wire up
			// something that only errors in the background -- every plugin this binary
			// registers initializes unconditionally with no --plugin-config (cleat-review
			// found exactly this: rate-limiter, oauth-provider, tenant-lifecycle, jobqueue
			// and scheduledbackup all run an immediate startup query). None of those should
			// print a permission error against a correctly-granted app login.
			if strings.Contains(out, "denied") || strings.Contains(out, "permission") {
				t.Fatalf("the app login could serve /healthz but something else failed with a permission error:\n%s", out)
			}
		})
	}
}

// TestMigrateOnlySucceedsWithADatabaseScopedLoginOnMySQLAndMSSQL is cleat-review's G2
// probe, kept as a permanent regression test: a migrate login scoped to its own
// database, with no server-level role, must still be able to run --migrate-only.
// PostgreSQL is not included -- its migrate login has needed CREATEROLE since before
// this PR (001_schema.sql's own CREATE ROLE cleat_app), which AWS documents the RDS
// master role as having by default, so there is no NEW requirement to regress here.
func TestMigrateOnlySucceedsWithADatabaseScopedLoginOnMySQLAndMSSQL(t *testing.T) {
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
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			ownerDSN, owner := deployScratch(t, c)
			scopedDSN := scopedMigrateLoginDSN(t, c.name, owner, ownerDSN)
			if code, out := runWorker(t, bin, nil, "--driver="+c.name, "--db="+scopedDSN, "--migrate-only"); code != 0 {
				t.Fatalf("--migrate-only exited %d with a database-scoped migrate login "+
					"(no server-level role) -- cleat#2203 must not raise this requirement:\n%s", code, out)
			}
		})
	}
}

// scopedMigrateLoginDSN creates a login scoped to ONLY the database deployScratch just
// built -- no server-level role -- and returns a DSN for it. This is the login shape
// cleat-review's G2 measured: db_owner on SQL Server, `ALL ON <db>.*` (plus `cleat\_%`,
// which MySQL has needed since before this PR for its own per-tenant-database replay,
// `cmd/cleat-worker/main.go`'s `if *driver == "mysql"` block) on MySQL.
func scopedMigrateLoginDSN(t *testing.T, dialect string, owner *sql.DB, ownerDSN string) string {
	t.Helper()
	pw := randomTestPassword(t)
	switch dialect {
	case "mysql":
		// log_bin_trust_function_creators: 003_procedures.sql's CREATE PROCEDURE needs it
		// under binary logging unless the connection is SUPER -- develop needs this for
		// ITS OWN migrate login already (unrelated to cleat#2203), and this test's whole
		// point is a login that is deliberately NOT SUPER. A stock MySQL 8 image with
		// binary logging on refuses 003 without this; set it explicitly rather than
		// assuming whatever happens to be configured outside this test.
		if _, err := owner.Exec(`SET GLOBAL log_bin_trust_function_creators = 1`); err != nil {
			t.Fatalf("setting log_bin_trust_function_creators: %v", err)
		}
		if _, err := owner.Exec(fmt.Sprintf(`CREATE USER IF NOT EXISTS 'cleat_2203_scoped_migrate'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
			t.Fatalf("creating a scoped migrate login: %v", err)
		}
		t.Cleanup(func() { _, _ = owner.Exec(`DROP USER IF EXISTS 'cleat_2203_scoped_migrate'@'%'`) })
		// IF NOT EXISTS makes the CREATE a no-op against a login a previous run's
		// t.Cleanup failed to drop (container reused across runs) -- without this ALTER,
		// that stale login keeps its OLD password while this run's DSN carries the NEW
		// one, which fails as "Access denied ... (using password: YES)", not as "does not
		// exist". Measured directly reproducing it against a long-lived container.
		if _, err := owner.Exec(fmt.Sprintf(`ALTER USER 'cleat_2203_scoped_migrate'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
			t.Fatalf("setting the scoped migrate login's password: %v", err)
		}
		cfg, err := gomysql.ParseDSN(ownerDSN)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(fmt.Sprintf("GRANT ALL ON `%s`.* TO 'cleat_2203_scoped_migrate'@'%%'", cfg.DBName)); err != nil {
			t.Fatalf("granting the scoped migrate login rights on its own database: %v", err)
		}
		if _, err := owner.Exec(`GRANT ALL ON ` + "`cleat\\_%`" + `.* TO 'cleat_2203_scoped_migrate'@'%'`); err != nil {
			t.Fatalf("granting the scoped migrate login rights on the tenant-database pattern: %v", err)
		}
		cfg.User = "cleat_2203_scoped_migrate"
		cfg.Passwd = pw
		return cfg.FormatDSN()
	default: // mssql
		// A bare CREATE LOGIN errors outright -- "already exists (15025)" -- against a
		// login a previous run's t.Cleanup failed to drop (container reused across runs),
		// unlike MySQL's IF NOT EXISTS. Guard it the same way mssqlAppRoleDSN already
		// does, and ALWAYS set the password afterwards so a stale login still picks up
		// this run's new one -- the same hermeticity gap just fixed above for MySQL.
		if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_2203_scoped_migrate')
			EXEC('CREATE LOGIN cleat_2203_scoped_migrate WITH PASSWORD = ''` + pw + `''')`); err != nil {
			t.Fatalf("creating a scoped migrate login: %v", err)
		}
		t.Cleanup(func() {
			_, _ = owner.Exec(`DROP USER IF EXISTS cleat_2203_scoped_migrate`)
			_, _ = owner.Exec(`DROP LOGIN cleat_2203_scoped_migrate`)
		})
		if _, err := owner.Exec(`ALTER LOGIN cleat_2203_scoped_migrate WITH PASSWORD = '` + pw + `'`); err != nil {
			t.Fatalf("setting the scoped migrate login's password: %v", err)
		}
		if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_2203_scoped_migrate')
			CREATE USER cleat_2203_scoped_migrate FOR LOGIN cleat_2203_scoped_migrate`); err != nil {
			t.Fatalf("mapping the scoped migrate login into this database: %v", err)
		}
		if _, err := owner.Exec(`IF NOT EXISTS (
				SELECT 1 FROM sys.database_role_members rm
				JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
				JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
				WHERE r.name = N'db_owner' AND m.name = N'cleat_2203_scoped_migrate'
			)
			ALTER ROLE db_owner ADD MEMBER cleat_2203_scoped_migrate`); err != nil {
			t.Fatalf("making the scoped migrate login db_owner: %v", err)
		}
		var sysadmin int
		if err := owner.QueryRow(`SELECT IS_SRVROLEMEMBER('sysadmin', 'cleat_2203_scoped_migrate')`).Scan(&sysadmin); err != nil {
			t.Fatalf("checking the scoped login is not sysadmin: %v", err)
		}
		if sysadmin == 1 {
			t.Fatal("cleat_2203_scoped_migrate is sysadmin -- the scratch server already grants it, so this probe proves nothing")
		}
		u, err := url.Parse(ownerDSN)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("cleat_2203_scoped_migrate", pw)
		return u.String()
	}
}

// deploymentSecretsInsert returns a two-placeholder (name, ciphertext) INSERT, one
// placeholder style per dialect, matching every other table-driven DSN helper in this
// package.
func deploymentSecretsInsert(dialect string) string {
	switch dialect {
	case "postgres":
		return `INSERT INTO deployment_secrets (name, ciphertext) VALUES ($1, $2)`
	case "mysql":
		return `INSERT INTO deployment_secrets (name, ciphertext) VALUES (?, ?)`
	default:
		return `INSERT INTO deployment_secrets (name, ciphertext) VALUES (@p1, @p2)`
	}
}

// writeProbeStmt is one ordinary, non-deployment_secrets, non-RLS statement per
// dialect that the app login MUST be able to run. plugin_defs: every column but
// `name`/`version` has a default, and it carries no tenant_id at all -- unlike
// tenant_settings/workflow_* (SQL Server's TenantFilter_Settings etc.), there is no
// row-level security here for a connection with no tenant context to run into, which
// would otherwise be indistinguishable from the privilege refusal this test exists to
// detect.
func writeProbeStmt(dialect string) string {
	switch dialect {
	case "postgres":
		return `INSERT INTO plugin_defs (name, version) VALUES ($1, '1')`
	case "mysql":
		return `INSERT INTO plugin_defs (name, version) VALUES (?, '1')`
	default:
		return `INSERT INTO plugin_defs (name, version) VALUES (@p1, '1')`
	}
}

// mysqlAppRoleDSN creates cleat_app with a real password directly (see this file's
// header for why it does not simply run deploy/mysql/900-app-role.sh), granting every
// table and routine QUERIED from information_schema -- not a fixed list -- for the same
// reason the deploy script queries rather than hardcodes: a fixed list of the 29 core
// tables already proved wrong once in this PR's own history, against tables plugin
// migrations add.
//
// grantTenantPattern adds the G1 `cleat\_%` wildcard grant -- needed to create or use a
// per-tenant database at all, but NOT passed by TestTheAppLoginCannotWriteDeploymentSecrets:
// deployScratch's own scratch databases are named `cleat_2117_deploy_<n>`, which matches
// `cleat\_%` (the exact collision deploy/mysql/900-app-role.sh's own guard exists to
// refuse in a real deployment), so granting it here would also grant cleat_app write on
// THIS database's own deployment_secrets and the write-restriction test would prove
// nothing. Only TestAWorkerBootsAndServesAsTheAppLoginOnEveryDialect, which asserts
// nothing about deployment_secrets, passes true.
func mysqlAppRoleDSN(t *testing.T, owner *sql.DB, ownerDSN string, grantTenantPattern bool) string {
	t.Helper()
	pw := randomTestPassword(t)
	cfg, err := gomysql.ParseDSN(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(fmt.Sprintf(`CREATE USER IF NOT EXISTS 'cleat_app'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
		t.Fatalf("creating cleat_app: %v", err)
	}
	// cleat_app is a SERVER-GLOBAL principal, not scoped to this test's scratch database --
	// cleat-review's N3: a subtest that grants it `cleat\_%` (grantTenantPattern) leaves that
	// grant in place for every OTHER test's scratch database too, since deployScratch names
	// them all `cleat_2117_deploy_<n>`, which matches. Measured: drop cleat_app, the write
	// test passes; run the boot test first (which grants the wildcard) without this cleanup,
	// then the write test fails. Dropping the whole user here means every test that creates
	// cleat_app also removes it, regardless of execution order between test FUNCTIONS, which
	// `grantTenantPattern` alone cannot fix since it only controls what THIS call grants.
	t.Cleanup(func() { _, _ = owner.Exec(`DROP USER IF EXISTS 'cleat_app'@'%'`) })
	if _, err := owner.Exec(fmt.Sprintf(`ALTER USER 'cleat_app'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
		t.Fatalf("setting cleat_app's password: %v", err)
	}

	ctx := context.Background()
	rows, err := owner.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'`, cfg.DBName)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatalf("%s has no tables -- has the migrate step run?", cfg.DBName)
	}
	for _, tbl := range tables {
		if _, err := owner.Exec(fmt.Sprintf("GRANT SELECT ON `%s` TO 'cleat_app'@'%%'", tbl)); err != nil {
			t.Fatalf("granting SELECT on %s: %v", tbl, err)
		}
		if tbl == "deployment_secrets" {
			continue
		}
		if _, err := owner.Exec(fmt.Sprintf("GRANT INSERT, UPDATE, DELETE ON `%s` TO 'cleat_app'@'%%'", tbl)); err != nil {
			t.Fatalf("granting write on %s: %v", tbl, err)
		}
	}

	routineRows, err := owner.QueryContext(ctx,
		`SELECT routine_name FROM information_schema.routines WHERE routine_schema = ?`, cfg.DBName)
	if err != nil {
		t.Fatalf("listing routines: %v", err)
	}
	var routines []string
	for routineRows.Next() {
		var name string
		if err := routineRows.Scan(&name); err != nil {
			routineRows.Close()
			t.Fatal(err)
		}
		routines = append(routines, name)
	}
	routineRows.Close()
	for _, r := range routines {
		if _, err := owner.Exec(fmt.Sprintf("GRANT EXECUTE ON PROCEDURE `%s` TO 'cleat_app'@'%%'", r)); err != nil {
			t.Fatalf("granting EXECUTE on %s: %v", r, err)
		}
	}

	if grantTenantPattern {
		// G1: the per-tenant-database pattern. See deploy/mysql/900-app-role.sh's own
		// comment on this exact grant for why it is safe for deployment_secrets in a
		// real deployment (and this function's own doc comment for why it is NOT safe
		// to pass true against deployScratch's own scratch database name).
		if _, err := owner.Exec("GRANT ALL PRIVILEGES ON `cleat\\_%`.* TO 'cleat_app'@'%'"); err != nil {
			t.Fatalf("granting cleat_app rights on the tenant-database pattern: %v", err)
		}
	}

	cfg.User = "cleat_app"
	cfg.Passwd = pw
	return cfg.FormatDSN()
}

// mssqlAppRoleDSN creates the cleat_app LOGIN (migrations/mssql/007_app_login.sql
// already created cleat_app_role, the database role carrying the GRANT/DENY) with a
// random password, enables it, and adds it to cleat_app_role -- see this file's header
// for why it does not simply run deploy/mssql/900-app-role.sh.
func mssqlAppRoleDSN(t *testing.T, owner *sql.DB, ownerDSN string) string {
	t.Helper()
	pw := randomTestPassword(t)
	if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_app')
		EXEC('CREATE LOGIN cleat_app WITH PASSWORD = ''` + pw + `''')`); err != nil {
		t.Fatalf("creating cleat_app login: %v", err)
	}
	if _, err := owner.Exec(`ALTER LOGIN cleat_app WITH PASSWORD = '` + pw + `'`); err != nil {
		t.Fatalf("setting cleat_app's password: %v", err)
	}
	if _, err := owner.Exec(`ALTER LOGIN cleat_app ENABLE`); err != nil {
		t.Fatalf("enabling cleat_app: %v", err)
	}
	if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app')
		CREATE USER cleat_app FOR LOGIN cleat_app`); err != nil {
		t.Fatalf("mapping cleat_app into this database: %v", err)
	}
	// cleat_app is a server-level LOGIN, so it survives this scratch database's own
	// cleanup. Unlike MySQL's wildcard (N3), SQL Server's GRANT/DENY lives on
	// cleat_app_role, which is scoped to one database -- so there is no cross-database
	// privilege leak here to worry about -- but the login itself still accumulates
	// across repeated local runs without this, which is a correctness problem the
	// moment two test runs disagree about its password. Drop the mapping in THIS
	// database before the login itself; best-effort, since another concurrent
	// scratch database could still be mapped to it.
	t.Cleanup(func() {
		_, _ = owner.Exec(`DROP USER IF EXISTS cleat_app`)
		_, _ = owner.Exec(`DROP LOGIN cleat_app`)
	})
	var roleExists int
	if err := owner.QueryRow(`SELECT COUNT(*) FROM sys.database_principals WHERE name = N'cleat_app_role' AND type = 'R'`).Scan(&roleExists); err != nil {
		t.Fatal(err)
	}
	if roleExists == 0 {
		t.Fatal("cleat_app_role does not exist -- has migrations/mssql/007_app_login.sql applied?")
	}
	if _, err := owner.Exec(`ALTER ROLE cleat_app_role ADD MEMBER cleat_app`); err != nil {
		t.Fatalf("adding cleat_app to cleat_app_role: %v", err)
	}
	u, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("cleat_app", pw)
	return u.String()
}

// TestTheMySQLDeployScriptRefusesACollidingDatabaseName proves
// deploy/mysql/900-app-role.sh's guard against a main database whose name also matches
// the tenant-database pattern cleat_app is granted ALL PRIVILEGES on. Needs no database
// connection and no `mysql` client (see this file's header for why that matters) --
// the guard is pure shell, checked before the script ever tries to reach a server, so
// an unreachable host and a deliberately wrong admin password are fine here: if the
// guard did not refuse first, this would fail differently (a connection error, not the
// guard's own message), which is the known-positive this test is built around.
func TestTheMySQLDeployScriptRefusesACollidingDatabaseName(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot, "deploy", "mysql", "900-app-role.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("deploy/mysql/900-app-role.sh: %v", err)
	}

	for _, tc := range []struct {
		name     string
		database string
		wantErr  bool
	}{
		{"collides", "cleat_production", true},
		{"collides with the test harness's own scratch name shape", "cleat_2117_deploy_123", true},
		{"does not collide", "cleat", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", script, "unreachable-host.invalid", "3306", "root", "x", tc.database)
			cmd.Env = append(cmd.Env, "CLEAT_APP_PASSWORD=x")
			out, err := cmd.CombinedOutput()
			refused := strings.Contains(string(out), "matches the tenant-database pattern")
			if tc.wantErr && !refused {
				t.Fatalf("expected the guard to refuse %q, got:\n%s", tc.database, out)
			}
			if !tc.wantErr && refused {
				t.Fatalf("the guard refused %q, which does not collide:\n%s", tc.database, out)
			}
			if !tc.wantErr && err == nil {
				t.Fatalf("expected the script to fail past the guard (no mysql client / unreachable host), got exit 0:\n%s", out)
			}
		})
	}
}
