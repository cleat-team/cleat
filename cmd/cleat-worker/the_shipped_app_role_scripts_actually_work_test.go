package main

// cleat#2939. deploy/{postgres,mysql,mssql}/900-app-role.sh are SHIPPED artifacts: an
// operator runs them to stand up the least-privilege cleat_app login that
// docs/how-to/use-deployment-secrets.md tells them a worker serves as. Until this file,
// no test executed any of them.
//
// That is not a theoretical gap. The Go helpers next door (pgAppRoleDSN,
// mysqlAppRoleDSN, mssqlAppRoleDSN) reimplement the same provisioning in Go, over
// database/sql, "matching, not calling, the shipped artifact" -- so the two can drift
// silently, and the shipped one is the one that actually runs in a deployment. It has
// already cost us once: cleat-review's N1 found deploy/mssql/900-app-role.sh shipping a
// password no operator could ever retrieve, and THREE earlier rounds of Go-based
// verification had passed it, because the Go double was correct while the shell was not.
//
// The reason given for the duplication was that the `mysql` and `sqlcmd` clients are not
// on the machine that runs `go test`. That is true of the CLIENTS, and false of the
// CONTAINERS: every dialect's CI job runs a container carrying its own client, and CI
// already reaches into one that way (`docker ps -q --filter "publish=5432"` then
// `docker exec "$CONTAINER" psql ...`, multi-db-ci.yml). So these tests pipe the SHIPPED
// SCRIPT into the container that serves the test's own DSN and run it there, which is the
// closest thing to a deployment this suite can express without installing the clients on
// the runner.
//
// WHAT THESE TESTS DO NOT COVER, stated rather than implied: the script's own argument
// parsing and env handling are exercised only as far as these call sites drive them, and
// docker-entrypoint-initdb.d's use of the PostgreSQL script (which runs it in a stock
// postgres image, where no Go binary exists) is not reproduced here. The point of this
// file is that the shipped SQL logic and its exit status are now measured, not reasoned
// about.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// appRoleContainer is everything needed to run one dialect's shipped script where its
// client is: the container serving this test's DSN, the admin credentials that script
// needs, and the port the database listens on INSIDE that container (which is not the
// published port the DSN names).
type appRoleContainer struct {
	container  string
	adminUser  string
	adminPass  string
	nativePort string
}

// nativePortFor is the port the server listens on inside its own container. The DSN names
// the host's PUBLISHED port, which differs whenever a deployment maps one (as every CI
// job here does), and the script runs inside the container, so it must be given the
// native one.
func nativePortFor(dialect string) string {
	switch dialect {
	case "postgres":
		return "5432"
	case "mysql":
		return "3306"
	default:
		return "1433"
	}
}

// containerForDSN finds the container publishing the port this test's DSN connects to,
// and returns its id. Discovery is by PUBLISHED PORT rather than by name or image,
// because that is the only thing the DSN and the container provably agree on -- and it is
// what multi-db-ci.yml already does for its own service containers.
//
// A missing docker binary skips: that is a genuine environmental precondition, the same
// class as an unset DSN. A DOCKER THAT WORKS BUT A CONTAINER THAT IS NOT FOUND IS A
// FAILURE, not a skip -- the DSN being set means this test was meant to run against a
// database, and a DSN this test cannot reach the client for is a broken setup, not an
// absent toolchain. That distinction is the one CLAUDE.md asks for: a skip is legitimate
// only for a missing precondition, never as a way to pass.
func containerForDSN(t *testing.T, publishedPort string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is not installed, so this test cannot run the shipped script where its client is " +
			"(the shipped artifact is the subject; skipping here is a missing toolchain, not a passing check)")
	}
	out, err := exec.Command("docker", "ps", "-q", "--filter", "publish="+publishedPort).Output()
	if err != nil {
		t.Fatalf("docker ps --filter publish=%s: %v", publishedPort, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		t.Fatalf("no container publishes port %s, but the DSN for this dialect points at it. This test "+
			"runs the SHIPPED deploy script inside the container that serves the DSN, so it cannot "+
			"proceed without one -- if this DSN names a remote server, the client is not reachable "+
			"from here and this test cannot make the claim it exists to make.", publishedPort)
	}
	return ids[0]
}

// publishedHostPort extracts the host:port a client DSN connects to, per dialect.
func publishedHostPort(t *testing.T, c deployDialect, dsn string) string {
	t.Helper()
	if c.name == "mysql" {
		cfg, err := gomysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parsing the MySQL DSN: %v", err)
		}
		return cfg.Addr
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the %s DSN: %v", c.name, err)
	}
	return u.Host
}

// credentialsOf splits an admin DSN into the user/password the script authenticates with.
func credentialsOf(t *testing.T, c deployDialect, dsn string) (user, pass string) {
	t.Helper()
	if c.name == "mysql" {
		cfg, err := gomysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parsing the MySQL DSN: %v", err)
		}
		return cfg.User, cfg.Passwd
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing the %s DSN: %v", c.name, err)
	}
	p, _ := u.User.Password()
	return u.User.Username(), p
}

// databaseNameOf reads the database name back out of a scratch DSN.
func databaseNameOf(t *testing.T, c deployDialect, dsn string) string {
	t.Helper()
	if c.name == "mysql" {
		cfg, err := gomysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.DBName
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if c.name == "postgres" {
		return strings.TrimPrefix(u.Path, "/")
	}
	return u.Query().Get("database")
}

func appRoleContainerFor(t *testing.T, c deployDialect, dsn string) appRoleContainer {
	t.Helper()
	hostPort := publishedHostPort(t, c, dsn)
	port := hostPort
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		port = hostPort[i+1:]
	}
	user, pass := credentialsOf(t, c, dsn)
	return appRoleContainer{
		container:  containerForDSN(t, port),
		adminUser:  user,
		adminPass:  pass,
		nativePort: nativePortFor(c.name),
	}
}

// runShippedAppRoleScript pipes deploy/<dialect>/900-app-role.sh into the container over
// stdin and runs it there, returning its exit code and combined output.
//
// stdin rather than a mount, because the containers this runs against are CI SERVICE
// containers: there is no way to give one a new bind mount after it has started, and
// `docker cp` would leave a copy of the script behind in the image.
//
// `bash -s --` reads the script from stdin and passes the remaining words as its
// positional parameters, which is exactly how the script's own usage documents them.
func runShippedAppRoleScript(t *testing.T, repoRoot string, c deployDialect, ac appRoleContainer, database, password string) (int, string) {
	t.Helper()
	script := filepath.Join(repoRoot, "deploy", c.name, "900-app-role.sh")
	f, err := os.Open(script)
	if err != nil {
		t.Fatalf("the shipped script this test exists to exercise is missing: %v", err)
	}
	defer f.Close()

	args := []string{"exec", "-i", "-e", "CLEAT_APP_PASSWORD=" + password}
	var positional []string
	switch c.name {
	case "postgres":
		// deploy/postgres/900-app-role.sh takes no arguments: it reads POSTGRES_USER and
		// POSTGRES_DB from the environment, because docker-entrypoint-initdb.d supplies
		// both to every script it runs.
		args = append(args, "-e", "POSTGRES_USER="+ac.adminUser, "-e", "POSTGRES_DB="+database)
	case "mysql":
		positional = []string{"localhost", ac.nativePort, ac.adminUser, ac.adminPass, database}
	default:
		positional = []string{"localhost", ac.adminUser, ac.adminPass, database}
	}
	args = append(args, ac.container, "bash", "-s", "--")
	args = append(args, positional...)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running %s in %s: %v", script, ac.container, err)
	return -1, ""
}

// appRoleDSN builds a client DSN for the login the shipped script just provisioned.
func appRoleDSN(t *testing.T, c deployDialect, ownerDSN, password string) string {
	t.Helper()
	if c.name == "mysql" {
		cfg, err := gomysql.ParseDSN(ownerDSN)
		if err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Passwd = "cleat_app", password
		return cfg.FormatDSN()
	}
	u, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("cleat_app", password)
	return u.String()
}

// dropAppLoginOnCleanup removes the login the script created where doing so is both
// possible and safe, mirroring what mysqlAppRoleDSN/mssqlAppRoleDSN already do for the
// same reason: cleat_app is a SERVER-global principal on MySQL and a server-level LOGIN on
// SQL Server, so it outlives the scratch database and accumulates across runs
// (cleat-review's N3). PostgreSQL is deliberately excluded -- its cleat_app is a
// CLUSTER-GLOBAL role created by the schema baseline, and dropping it here would take it
// away from the shared `cleat` database this suite's other tests run against.
// pgAppRoleDSN does not drop it either, for that same reason.
func dropAppLoginOnCleanup(t *testing.T, c deployDialect, owner *sql.DB) {
	t.Helper()
	if c.name == "postgres" {
		return
	}
	t.Cleanup(func() {
		if c.name == "mssql" {
			// The login is what persists, and DROP LOGIN fails while a database user is
			// mapped to it, so the mapping goes first -- the order mssqlAppRoleDSN uses.
			_, _ = owner.Exec(`DROP USER IF EXISTS cleat_app`)
			_, _ = owner.Exec(`DROP LOGIN cleat_app`)
			return
		}
		_, _ = owner.Exec(`DROP USER IF EXISTS 'cleat_app'@'%'`)
	})
}

// TestTheShippedAppRoleScriptsProvisionAWorkingLogin runs each dialect's SHIPPED deploy
// script against a migrated scratch database, inside the container that carries the
// dialect's client, and then connects as the login that script created -- not as one a Go
// helper created -- and asserts the restriction the script exists to establish.
//
// The assertions are the shape of TestTheAppLoginCannotWriteDeploymentSecrets, and
// deliberately so: what is new here is only WHO PROVISIONED the login. If the script and
// the Go double ever disagree, this test is the one that sees it, because it is the only
// one that runs the script.
func TestTheShippedAppRoleScriptsProvisionAWorkingLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the shipped deploy scripts inside database containers")
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
			ac := appRoleContainerFor(t, c, c.admin())

			// The scratch database must NOT be named with deployScratch's own
			// `cleat_2117_deploy_` prefix: deploy/mysql/900-app-role.sh REFUSES any
			// database matching `cleat_*`, by a guard that exists precisely because such a
			// name collides with the tenant-database wildcard grant. Running the script
			// against the harness's usual scratch name would fail before reaching any SQL,
			// and the failure would be the guard working correctly.
			ownerDSN, owner := deployScratchNamed(t, c, "approle_2939_")
			if code, out := runWorker(t, bin, nil, "--driver="+c.name, "--db="+ownerDSN, "--migrate-only"); code != 0 {
				t.Fatalf("--migrate-only exited %d:\n%s", code, out)
			}
			dropAppLoginOnCleanup(t, c, owner)
			dbName := databaseNameOf(t, c, ownerDSN)

			password := randomTestPassword(t)

			code, out := runShippedAppRoleScript(t, repoRoot, c, ac, dbName, password)
			if code != 0 {
				t.Fatalf("the shipped deploy script exited %d against a freshly migrated database. "+
					"It is the artifact an operator runs, so this is a deployment failure, not a test "+
					"setup one:\n%s", code, out)
			}
			if !strings.Contains(out, "cleat_app is ready") {
				t.Fatalf("the script exited 0 but never printed its success line, so this test cannot tell "+
					"whether it did the work or silently did nothing:\n%s", out)
			}

			// IDEMPOTENCY. Both MySQL's and SQL Server's scripts document re-running as the
			// supported way to pick up a newly enabled plugin's grants, so a second run must
			// leave a login that still authenticates with the SAME password. Two real defects
			// found while this work was in review live exactly here: SQL Server's bare
			// `CREATE LOGIN` errored (15025) against a login left by a previous run, and
			// MySQL's `CREATE USER IF NOT EXISTS` silently left a stale password in place.
			// Both are invisible to a test that runs the script only once.
			code, out = runShippedAppRoleScript(t, repoRoot, c, ac, dbName, password)
			if code != 0 {
				t.Fatalf("re-running the shipped deploy script exited %d, but re-running is the documented "+
					"way to pick up a new plugin's grants:\n%s", code, out)
			}

			app, err := sql.Open(c.driver, appRoleDSN(t, c, ownerDSN, password))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.Close() })
			if err := app.Ping(); err != nil {
				t.Fatalf("the login the shipped script created could not connect, so re-running it did not "+
					"leave a working login: %v", err)
			}

			insertSecret, writeProbe := deploymentSecretsInsert(c.name), writeProbeStmt(c.name)

			// KNOWN-POSITIVE, as the owner/migrate login: both statements must succeed on a
			// freshly migrated database, or a refusal below proves nothing about the script's
			// login specifically.
			if _, err := owner.Exec(insertSecret, "known-positive", "ct"); err != nil {
				t.Fatalf("the owner login could not insert into deployment_secrets on a fresh database "+
					"(is the schema baseline applied?): %v", err)
			}
			if _, err := owner.Exec(writeProbe, "cleat_2939_probe_owner"); err != nil {
				t.Fatalf("the owner login could not write the ordinary table this test uses as a negative "+
					"control: %v", err)
			}

			// THE REFUSAL: the login the shipped script provisioned must not write
			// deployment_secrets.
			if _, err := app.Exec(insertSecret, "should-be-refused", "ct"); err == nil {
				t.Fatal("the login provisioned by the shipped deploy script inserted into " +
					"deployment_secrets, so the script does not establish the restriction it exists for")
			}

			// NEGATIVE CONTROL: the login is not broadly broken. Without this, "refused" above
			// could mean "this login cannot write anything", which is not what ships.
			if _, err := app.Exec(writeProbe, "cleat_2939_probe_app"); err != nil {
				t.Fatalf("the login provisioned by the shipped deploy script could not write an ordinary "+
					"table a worker serves on, so the script's grant is too narrow: %v", err)
			}
		})
	}
}

// TestTheShippedAppRoleScriptsRefuseToReportSuccessWhenTheyFailed is the known-positive
// for the test above, and it exists because the test above could not have caught the
// defect it was written around.
//
// Writing it found that deploy/mssql/900-app-role.sh printed
//
//	Msg 15151 ... Cannot alter the role 'cleat_app_role', because it does not exist
//
// followed by its own "cleat_app is ready ... a member of cleat_app_role" line, and exited
// 0 -- because sqlcmd reports SQL errors through ERRORLEVEL only when given -b. Against
// the happy path above the role always exists, so that test passes straight over it.
// This one drives each script at a state where its work CANNOT succeed and requires a
// non-zero exit with no success line.
//
// THE TRIGGER IS PER DIALECT, and it has to be, because "cannot succeed" is a property of
// the dialect's principal model rather than of the script. PostgreSQL's cleat_app is a
// CLUSTER-GLOBAL role created by the schema baseline in whichever database ran it first,
// so pointing its script at a fresh, empty database does NOT fail -- measured: it exits 0
// and says "cleat_app is ready", correctly, because its actual job (give the existing
// role a login) did succeed. Its constructible failure is a database that does not exist.
// MySQL and SQL Server both create their principals from the script itself, so an
// unmigrated database is the state where their work cannot complete -- for SQL Server
// because cleat_app_role is missing, which is exactly the case the -b defect reported
// success over.
//
// Without a test like this, "the shipped script exits 0" is a check that cannot fail for
// the reason that matters: it is satisfied by a script that reports success
// unconditionally.
func TestTheShippedAppRoleScriptsRefuseToReportSuccessWhenTheyFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the shipped deploy scripts inside database containers")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
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
			ac := appRoleContainerFor(t, c, c.admin())

			var dbName string
			if c.name == "postgres" {
				// Never created, and never migrated: the script's psql cannot connect to it
				// at all, so no part of its work can succeed.
				dbName = fmt.Sprintf("approle_2939_absent_%d", time.Now().UnixNano()%1_000_000_000)
			} else {
				// Created but deliberately NOT migrated. MySQL's script finds no tables to
				// grant on; SQL Server's finds no cleat_app_role to add the login to. Both are
				// the state a deployment is in when the migrate step has not run yet, which is
				// exactly when a false success is most expensive: the operator is told the app
				// login is ready and it is not.
				ownerDSN, owner := deployScratchNamed(t, c, "approle_2939_unmigrated_")
				dropAppLoginOnCleanup(t, c, owner)
				dbName = databaseNameOf(t, c, ownerDSN)
			}

			code, out := runShippedAppRoleScript(t, repoRoot, c, ac, dbName, randomTestPassword(t))

			if code == 0 {
				t.Fatalf("the shipped deploy script exited 0 against a state where its work cannot succeed, "+
					"so it reports success unconditionally. An operator would be told the app login is "+
					"ready when it is not:\n%s", out)
			}
			if strings.Contains(out, "cleat_app is ready") {
				t.Fatalf("the shipped deploy script exited %d but still printed its success line, which is "+
					"the false report this test exists to prevent:\n%s", code, out)
			}
		})
	}
}
