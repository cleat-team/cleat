package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The pair of tests below is the falsification for IMPROVEMENT-PLAN 1.10.
// They run the same check against two connections to the *same database* and
// require opposite answers, so neither can pass by accident: if the check
// always said "enforced" the first would fail, and if it always said
// "bypassed" the second would.

// TestCheckRLSEnforced_DetectsSuperuserBypass runs against the connection
// every shipped configuration actually used -- CLEAT_TEST_DB, which is a
// superuser, exactly as docker-compose.cluster.yml's POSTGRES_USER=cleat and
// CI's `postgres` are. It must report the bypass.
func TestCheckRLSEnforced_DetectsSuperuserBypass(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	reasons, err := CheckRLSEnforced(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckRLSEnforced: %v", err)
	}
	if len(reasons) == 0 {
		t.Fatal("no bypass reported for the owner/superuser connection: either the " +
			"check is broken, or CLEAT_TEST_DB is unexpectedly an unprivileged role")
	}

	// The message has to name the actual mechanism, or an operator cannot act
	// on it.
	msg := FormatRLSBypass(reasons)
	for _, want := range []string{"row-level security", "005_app_role.sql", "--migrate-db"} {
		if !strings.Contains(msg, want) {
			t.Errorf("bypass message does not mention %q:\n%s", want, msg)
		}
	}
	t.Logf("reported: %s", msg)
}

// TestCheckRLSEnforced_PassesForUnprivilegedRole runs against a role that is
// neither superuser nor owner -- what a deployment is supposed to connect as
// after 005_app_role.sql. The check must find nothing.
func TestCheckRLSEnforced_PassesForUnprivilegedRole(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()

	reasons, err := CheckRLSEnforced(context.Background(), appDB)
	if err != nil {
		t.Fatalf("CheckRLSEnforced: %v", err)
	}
	if len(reasons) != 0 {
		t.Errorf("unprivileged role reported as bypassing RLS, so a correctly "+
			"configured deployment would be refused:\n%s", FormatRLSBypass(reasons))
	}
}

// TestCheckRLSEnforced_ReportsMissingPolicies guards the vacuity case: a
// database with no policies passes every other test in this file while
// isolating nothing, which is precisely the state the deleted root schema.sql
// produced.
func TestCheckRLSEnforced_ReportsMissingPolicies(t *testing.T) {
	db := bootstrapScratchDB(t, "cleat_rls_check_nopolicy_test")

	if _, err := db.Exec(`
		DO $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT n.nspname, c.relname, p.polname
			         FROM pg_policy p
			         JOIN pg_class c ON c.oid = p.polrelid
			         JOIN pg_namespace n ON n.oid = c.relnamespace
			LOOP
				EXECUTE format('DROP POLICY %I ON %I.%I', r.polname, r.nspname, r.relname);
			END LOOP;
		END $$;`); err != nil {
		t.Fatalf("drop policies: %v", err)
	}

	reasons, err := CheckRLSEnforced(context.Background(), db)
	if err != nil {
		t.Fatalf("CheckRLSEnforced: %v", err)
	}
	var found bool
	for _, r := range reasons {
		if r.Kind == "no_policies" {
			found = true
		}
	}
	if !found {
		t.Errorf("a database with every policy dropped was not reported as having none; got %+v", reasons)
	}
}

func TestFormatRLSBypass_EmptyForNoReasons(t *testing.T) {
	if got := FormatRLSBypass(nil); got != "" {
		t.Errorf("FormatRLSBypass(nil) = %q, want empty", got)
	}
}
