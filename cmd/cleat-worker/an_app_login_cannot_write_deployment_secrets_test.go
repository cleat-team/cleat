package main

// cleat#2203. PostgreSQL's cleat_app has had SELECT-only access to deployment_secrets
// since the baseline (migrations/postgres/001_schema.sql); MySQL and SQL Server had no
// application login at all, so the one login every worker used could write it too
// (docs/how-to/use-deployment-secrets.md used to say exactly that -- "no equivalent role
// split to revoke from"). migrations/{mysql,mssql}/007_app_login.sql adds cleat_app to
// both. This proves the new logins on real databases rather than reading the SQL and
// trusting it: a GRANT that silently failed, or a MySQL statement that (per the
// migration's own comment) accidentally granted at the database level instead of per
// table, would still read as correct from the file.
//
// Each dialect gets its OWN assertion shape, not a shared helper pretending the two
// privilege models are the same thing: MySQL has no DENY, so the proof is "no grant for
// write was ever issued"; SQL Server's DENY overrides a broader GRANT regardless of
// order, so the proof is the DENY actually refusing an operation its schema-level GRANT
// would otherwise allow. Postgres is included as a known-positive: if the harness
// (deployScratch, pgAppRoleDSN, runWorker) cannot see PostgreSQL's existing, already
// shipped restriction, it cannot be trusted to see the two new ones either.
//
// Each case also proves two things a passing INSERT refusal alone would not:
//   - the SAME login can still write an ordinary table (otherwise "denied" might mean
//     "this login cannot write anything", which is not what shipped)
//   - the owner/migrate login CAN still write deployment_secrets (otherwise the
//     cleatctl set-deployment-secret / retire-deployment-secret path this leaves
//     untouched would itself be broken, and a reader could not tell from this test)

import (
	"database/sql"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"
)

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
				appDSN = mysqlAppRoleDSN(t, owner, ownerDSN)
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

// mysqlAppRoleDSN gives the MySQL cleat_app login (migrations/mysql/007_app_login.sql)
// a password and unlocks it -- the ACCOUNT LOCK equivalent of PostgreSQL's NOLOGIN,
// see pgAppRoleDSN -- and returns the DSN the worker connects with.
func mysqlAppRoleDSN(t *testing.T, owner *sql.DB, ownerDSN string) string {
	t.Helper()
	if _, err := owner.Exec(`ALTER USER 'cleat_app'@'%' IDENTIFIED BY 'cleat-2203-pw' ACCOUNT UNLOCK`); err != nil {
		t.Fatalf("giving cleat_app a password (is the schema baseline applied?): %v", err)
	}
	cfg, err := mysql.ParseDSN(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User = "cleat_app"
	cfg.Passwd = "cleat-2203-pw"
	return cfg.FormatDSN()
}

// mssqlAppRoleDSN gives the SQL Server cleat_app login
// (migrations/mssql/007_app_login.sql) a password and ENABLEs it -- it is created
// DISABLED, the same NOLOGIN-shaped gap pgAppRoleDSN documents for PostgreSQL -- and
// returns the DSN the worker connects with.
func mssqlAppRoleDSN(t *testing.T, owner *sql.DB, ownerDSN string) string {
	t.Helper()
	if _, err := owner.Exec(`ALTER LOGIN cleat_app WITH PASSWORD = 'cleat-2203-Pw!'`); err != nil {
		t.Fatalf("setting cleat_app's password (is the schema baseline applied?): %v", err)
	}
	if _, err := owner.Exec(`ALTER LOGIN cleat_app ENABLE`); err != nil {
		t.Fatalf("enabling cleat_app: %v", err)
	}
	u, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("cleat_app", "cleat-2203-Pw!")
	return u.String()
}
