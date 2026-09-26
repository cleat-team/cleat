package engine

import (
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTheBaselineCreatesTheClusterScopedObjects asserts that applying the
// shipped PostgreSQL baseline leaves the CLUSTER-SCOPED objects in place.
//
// WHY THIS EXISTS SEPARATELY FROM EVERY OTHER SCHEMA CHECK. The compaction
// that produced 001/002/003 was verified by a differential harness that builds
// two databases and compares their catalogs -- and a per-database catalog
// cannot carry a cluster-level fact. pg_dump --schema-only emits nothing for
// pg_authid (the roles), pg_auth_members (the memberships) or pg_shdescription
// (a role's comment), so a compaction can DROP every one of them and still
// produce a byte-identical per-database diff. cleat#2416 lost two cleat_sweep
// memberships exactly that way, and the harness reported clean.
//
// The cost of that silence is not theoretical: SET LOCAL ROLE cleat_sweep is
// how the cross-tenant plugin sweep runs, and it fails with a privilege error
// the moment the membership is gone. Nothing else in this tree asserts on
// pg_auth_members -- checked, zero hits -- so until this test the membership
// was covered only by another test happening to use it.
//
// The failure messages name the object AND the reason the ordinary check is
// blind to it, so a red run here does not send someone to the catalog diff
// that will report clean.
//
// ONE PROPERTY TO KNOW BEFORE DEBUGGING A RED RUN HERE: this asserts a
// CLUSTER-WIDE fact, so it reports drift that no migration will repair on its
// own. SetupFullSchema goes through the migration runner, which records 001 in
// schema_migrations and therefore does not re-run it -- so a role, membership
// or comment that went missing stays missing, and re-running this test reports
// it again rather than healing. That is deliberate, and it is the honest
// reading: if the membership is absent, SET LOCAL ROLE cleat_sweep is broken
// for every caller right now. It also means the check is not free of the
// shared-cluster hazard the rest of this package documents -- the state it
// reads can be changed by any other process attached to the same server.
func TestTheBaselineCreatesTheClusterScopedObjects(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// The roles, with the attributes the deployment depends on.
	want := map[string]struct {
		bypassRLS bool
		canLogin  bool
	}{
		"cleat_app":        {false, false},
		"cleat_sweep":      {false, false},
		"cleat_dispatcher": {true, false},
	}
	for name, attrs := range want {
		var super, bypass, login bool
		err := db.QueryRow(`SELECT rolsuper, rolbypassrls, rolcanlogin
		    FROM pg_roles WHERE rolname = $1`, name).Scan(&super, &bypass, &login)
		if err != nil {
			t.Errorf("role %s is absent after the baseline applied: %v.\n\n"+
				"A per-database catalog diff cannot see this -- pg_authid is "+
				"cluster-wide -- so a compaction that dropped CREATE ROLE reports "+
				"clean.", name, err)
			continue
		}
		if super {
			t.Errorf("role %s is a SUPERUSER; the baseline creates it NOSUPERUSER", name)
		}
		if bypass != attrs.bypassRLS {
			t.Errorf("role %s has rolbypassrls=%v, want %v", name, bypass, attrs.bypassRLS)
		}
		if login != attrs.canLogin {
			t.Errorf("role %s has rolcanlogin=%v, want %v", name, login, attrs.canLogin)
		}
	}

	// The memberships. inherit_option must be FALSE: 077 measured that a plain
	// GRANT lets the application role read all 400000 rows of another tenant's
	// data with no error, while WITH INHERIT FALSE returns 1000. That flag is
	// the difference between "may SET ROLE to sweep" and "sweeps passively".
	var members int
	var inherit bool
	if err := db.QueryRow(`
		SELECT count(*), COALESCE(bool_and(m.inherit_option), false)
		  FROM pg_auth_members m
		  JOIN pg_roles g ON g.oid = m.roleid
		  JOIN pg_roles r ON r.oid = m.member
		 WHERE g.rolname = 'cleat_sweep' AND r.rolname = 'cleat_app'`).
		Scan(&members, &inherit); err != nil {
		t.Fatalf("reading cleat_sweep's memberships: %v", err)
	}
	if members == 0 {
		t.Errorf("cleat_app is not a member of cleat_sweep. SET LOCAL ROLE " +
			"cleat_sweep -- how every cross-tenant plugin sweep runs -- cannot " +
			"succeed without it, and pg_auth_members is cluster-wide, so the " +
			"per-database catalog diff cannot report this.")
	} else if inherit {
		t.Errorf("cleat_app's membership in cleat_sweep has inherit_option=true. " +
			"077 measured that a plain GRANT lets the application role read every " +
			"tenant's rows with no error; WITH INHERIT FALSE is the whole " +
			"distinction and it is silent when wrong.")
	}

	// The role comment. pg_shdescription is a SHARED catalog, so this is the
	// third thing a per-database dump does not carry.
	var comment *string
	if err := db.QueryRow(`
		SELECT d.description FROM pg_shdescription d JOIN pg_roles r ON r.oid = d.objoid
		 WHERE r.rolname = 'cleat_sweep'`).Scan(&comment); err != nil {
		t.Errorf("cleat_sweep carries no role comment: %v.\n\n"+
			"COMMENT ON ROLE lands in pg_shdescription, which is cluster-wide "+
			"and absent from a per-database dump.", err)
	} else if comment == nil || *comment == "" {
		t.Error("cleat_sweep's role comment is empty; the baseline sets one explaining " +
			"how the role is entered")
	}
}
