package engine

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// managedRolePassword is the login for the RDS-shaped role this test creates.
// Not a secret: the role exists for the length of one test against a test
// database, and it is dropped afterwards.
const managedRolePassword = "cleat-managed-test-pw"

// The whole migration set applies on managed PostgreSQL, where no superuser
// exists.
//
// # Why a role of this exact shape
//
// AWS documents the RDS master user as
//
//	CREATE ROLE postgres WITH LOGIN NOSUPERUSER INHERIT CREATEDB CREATEROLE
//	  NOREPLICATION VALID UNTIL 'infinity'
//
// so this is that definition rather than an approximation of it. Cloud SQL's
// cloudsqlsuperuser and Azure's azure_pg_admin are equivalent in the way that
// matters here: highest-privileged, and not a superuser.
//
// # What it caught
//
// Measured on PostgreSQL 16 before the fixes this test guards, 8 of 72
// migrations failed as this role, from three independent causes:
//
//   - 005 re-asserted NOSUPERUSER unconditionally, and PostgreSQL checks the
//     privilege to change an attribute rather than whether the value changes.
//   - 023/024 create a BYPASSRLS role, which only a superuser can do; 040 and
//     073 then failed on the missing role.
//   - 085/086/089 backfill over RLS-forced tables and were relying on the
//     migrating connection being a superuser, which the comment in 085 states
//     outright.
//
// The first of those aborted the run at file 5, so cleat could not be
// INSTALLED on RDS at all -- not merely run single-tenant there.
func TestTheMigrationSetAppliesWithoutASuperuser(t *testing.T) {
	// NOT bootstrapScratchDB: that helper applies the whole schema as the
	// admin connection before handing the database back, so the schema would
	// already exist and be owned by a superuser. What this test needs is an
	// EMPTY database owned by the managed role, because the question is
	// whether that role can build the schema at all.
	adminDSN := testutilPostgresDSN(t)
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("opening the admin connection: %v", err)
	}
	defer admin.Close()
	// "Configured and unreachable" is a FAILURE, not a skip -- the distinction
	// bootstrapScratchDB draws for the same reason, and the one
	// scripts/check-skips.sh exists to keep: a skip is indistinguishable from a
	// pass, so a CI service that quietly stopped resolving would read as this
	// test having run.
	if err := admin.Ping(); err != nil {
		if postgresConfiguredForTest() {
			t.Fatalf("configured postgres database at %s is unreachable: %v",
				redactPostgresDSN(adminDSN), err)
		}
		t.Skipf("no postgres available (default DSN, none configured): %v", err)
	}

	const scratch = "cleat_managed_pg_test"

	// The role is cluster-wide, so a previous run's copy is dropped first.
	// REASSIGN before DROP: a failed earlier run can leave it owning objects.
	roleName := "cleat_managed_test_role"
	for _, stmt := range []string{
		fmt.Sprintf(`DROP ROLE IF EXISTS %s`, roleName),
		fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER INHERIT CREATEDB CREATEROLE NOREPLICATION PASSWORD '%s'`,
			roleName, managedRolePassword),
	} {
		if _, err := admin.Exec(stmt); err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("preparing the managed-PostgreSQL role: %v\nstatement: %s", err, stmt)
		}
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(fmt.Sprintf(`DROP OWNED BY %s CASCADE`, roleName))
		_, _ = cleanup.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, roleName))
	})

	// ROLES ARE CLUSTER-WIDE, AND THIS TEST DOES NOT GET A FRESH CLUSTER.
	//
	// migrations create cleat_app, cleat_sweep and cleat_dispatcher at the
	// server level, so a cluster where any other test has run already has them
	// -- owned by whoever created them, usually the superuser these tests
	// normally connect as. 077 then fails for the managed role with
	//
	//   ERROR:  permission denied
	//   DETAIL:  The current user must have the ADMIN option on role "cleat_sweep".
	//
	// which is an artifact of sharing a cluster rather than anything a managed
	// deployment would see: on RDS the master role creates those roles itself
	// and holds ADMIN on them automatically. Granting ADMIN here reproduces
	// that state instead of papering over it, and does so WITHOUT dropping the
	// roles, which another test may be using concurrently.
	//
	// (A real deployment that applied migrations as a superuser and later
	// switched to a non-superuser role would hit the genuine version of this.
	// That is a privilege downgrade mid-life, a different scenario from the one
	// under test here, and it is not addressed.)
	existing, err := admin.Query(`SELECT rolname FROM pg_roles WHERE rolname LIKE 'cleat!_%' ESCAPE '!'`)
	if err != nil {
		t.Fatalf("listing pre-existing cleat roles: %v", err)
	}
	var preexisting []string
	for existing.Next() {
		var name string
		if err := existing.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		preexisting = append(preexisting, name)
	}
	_ = existing.Close()
	for _, name := range preexisting {
		if name == roleName {
			continue // the test's own role, which matches the same prefix
		}
		// cleat_dispatcher is EXCLUDED, and that is the point rather than an
		// oversight. On managed PostgreSQL it cannot exist at all, so the
		// managed role is certainly not a member of it; granting ADMIN here
		// would let 023's owner change succeed and the assertions below would
		// then be describing a database no RDS deployment can have.
		//
		// KNOWN LIMIT OF THIS TEST: when the role already exists on the cluster
		// -- which it does whenever another test in this package has run --
		// 023's CREATE ROLE branch is skipped, so the exception handler in it
		// is not exercised here. What IS exercised either way is the end state
		// the handler exists to produce, asserted below. The handler itself was
		// verified against a cluster with no cleat_* roles at all, where the
		// full set applies 0-of-72 failing.
		if name == "cleat_dispatcher" {
			continue
		}
		if _, err := admin.Exec(fmt.Sprintf(`GRANT %s TO %s WITH ADMIN OPTION`, name, roleName)); err != nil {
			t.Fatalf("granting ADMIN on the pre-existing role %s: %v", name, err)
		}
	}

	// The role OWNS the database, as an RDS master user owns the ones it
	// creates. Ownership matters to what is being tested: the backfill fix
	// restores the OWNER's exemption, so a database the role did not own would
	// exercise a different path from the one a deployment takes.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratch); err != nil {
		t.Fatalf("dropping the scratch database: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, scratch, roleName)); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + scratch)
	})

	managedDSN := swapCredentials(t, mustSwapDatabase(t, adminDSN, scratch),
		roleName, managedRolePassword)
	db, err := sql.Open("postgres", managedDSN)
	if err != nil {
		t.Fatalf("opening the managed-role connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("the managed role cannot connect: %v", err)
	}

	// CONFIRM THE PREMISE rather than assuming it. If this connection turned
	// out to be a superuser, or to hold BYPASSRLS, every assertion below would
	// pass for the wrong reason and the test would prove nothing -- which is
	// exactly how the defects it guards survived, since every other database
	// test here connects as the owner, and on PostgreSQL that is a superuser.
	var isSuper, hasBypass bool
	if err := db.QueryRow(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&isSuper, &hasBypass); err != nil {
		t.Fatalf("checking the role's attributes: %v", err)
	}
	if isSuper || hasBypass {
		t.Fatalf("the test role is superuser=%v bypassrls=%v; it must be neither, "+
			"or this test cannot observe what a managed deployment observes", isSuper, hasBypass)
	}

	ctx := context.Background()
	run := func(pass string) {
		t.Helper()
		// The runner appends the dialect directory itself, so it wants the
		// parent of what migrationsDir returns.
		r := migration.NewRunner(db, migration.DialectPostgres, filepath.Dir(migrationsDir(t)))
		if err := r.Run(ctx); err != nil {
			t.Fatalf("%s: the migration set does not apply as a non-superuser: %v", pass, err)
		}
	}

	// TWICE. Re-applying the whole set is what an operator upgrading does, and
	// the first fix for the owner changes passed a fresh run and failed a
	// second one: it guarded on "does cleat_dispatcher exist", while
	// ALTER ... OWNER TO also requires membership, so a role that existed but
	// belonged to somebody else still raised 42501.
	run("first pass")
	run("second pass")

	t.Run("row-level security is still forced everywhere", func(t *testing.T) {
		// THE SECURITY ASSERTION. The backfill fix drops FORCE on a handful of
		// tables for the length of a statement and restores it. A restore that
		// silently did not happen would leave the table OWNER exempt from its
		// own policies for good -- invisible to every other test here, because
		// they connect as a superuser who is exempt regardless.
		rows, err := db.Query(`
			SELECT n.nspname || '.' || c.relname
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relrowsecurity AND NOT c.relforcerowsecurity
			   AND n.nspname IN ('public', 'admin')
			 ORDER BY 1`)
		if err != nil {
			t.Fatalf("checking FORCE ROW LEVEL SECURITY: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var unforced []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			unforced = append(unforced, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("checking FORCE ROW LEVEL SECURITY: %v", err)
		}
		if len(unforced) > 0 {
			t.Errorf("row-level security is enabled but not FORCEd on: %s\n"+
				"The owner of these tables is exempt from their own policies. A backfill "+
				"that drops FORCE must restore it.", strings.Join(unforced, ", "))
		}

		// Control: the query above returns nothing on a database with no RLS at
		// all, so it must also be shown that there IS some.
		var forced int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relrowsecurity AND c.relforcerowsecurity
			   AND n.nspname IN ('public', 'admin')`).Scan(&forced); err != nil {
			t.Fatalf("counting forced tables: %v", err)
		}
		if forced == 0 {
			t.Error("no table has FORCE ROW LEVEL SECURITY set, so the assertion above " +
				"passed vacuously")
		}
	})

	t.Run("the cross-tenant function exists and reports itself unavailable", func(t *testing.T) {
		// 023 degrades rather than aborting, and what it leaves behind is
		// deliberate: the function exists, owned by the migrating role, so
		// CheckCrossTenantCapability reports "its owner does not have
		// BYPASSRLS" -- true, and a better thing to tell an RDS operator than
		// "apply 023", a file they cannot apply.
		var owner string
		var ownerBypass bool
		err := db.QueryRow(`
			SELECT r.rolname, r.rolbypassrls
			  FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner
			  JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = 'admin' AND p.proname = 'claim_workflows'`).Scan(&owner, &ownerBypass)
		if err != nil {
			t.Fatalf("admin.claim_workflows was not created at all: %v", err)
		}
		if ownerBypass {
			t.Errorf("admin.claim_workflows is owned by %s, which has BYPASSRLS; "+
				"that cannot happen on a platform where no superuser could grant it", owner)
		}

		cap := NewPostgresStore(db).CheckCrossTenantCapability(ctx)
		if cap.Claim {
			t.Error("the capability probe reports the cross-tenant claim as available " +
				"on a database whose function owner has no exemption")
		}
		if !strings.Contains(cap.ClaimReason, "BYPASSRLS") {
			t.Errorf("the probe's reason does not name the missing attribute, so an "+
				"operator cannot tell this from a missing migration: %q", cap.ClaimReason)
		}
	})
}

// swapCredentials rewrites the user and password of a postgres DSN.
func swapCredentials(t *testing.T, dsn, user, password string) string {
	t.Helper()
	scheme := strings.Index(dsn, "://")
	at := strings.Index(dsn, "@")
	if scheme < 0 || at < 0 {
		t.Fatalf("cannot derive a role DSN from %q", redactPostgresDSN(dsn))
	}
	return dsn[:scheme+3] + user + ":" + password + dsn[at:]
}

func mustSwapDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	out, err := swapDatabase(dsn, name)
	if err != nil {
		t.Fatalf("derive DSN for %s: %v", name, err)
	}
	return out
}
