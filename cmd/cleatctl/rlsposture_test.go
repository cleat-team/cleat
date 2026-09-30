package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// openMSSQLMockDB returns a *sql.DB backed by the same positional mock driver
// checkdb_test.go uses, so mssqlPostureOf's own query sequence can be driven
// directly without a real SQL Server connection.
func openMSSQLMockDB(script []checkDBResult) *sql.DB {
	current := 0
	connector := &checkDBMockConnector{script: script, current: &current}
	return sql.OpenDB(connector)
}

// TestMSSQLPostureOfConsultsThePredicateForm falsifies cleat#2760:
// mssqlPostureOf used to classify a login as exempt from IS_ROLEMEMBER alone,
// so a member under the DEFAULT ('plain') predicate -- which does not
// reference cleat_admin at all (cleat#1541) -- was misreported as exempt when
// membership grants it nothing. Every case here also checks that the
// SECOND query (the predicate-form read) is issued only when membership
// actually makes it relevant -- a non-member script has only one scripted
// result, so an extra query would fail the mock driver with "unexpected
// query", not merely go unchecked.
func TestMSSQLPostureOfConsultsThePredicateForm(t *testing.T) {
	ctx := context.Background()

	t.Run("member under the admin predicate is exempt", func(t *testing.T) {
		db := openMSSQLMockDB([]checkDBResult{
			makeQueryResult([]string{""}, []driver.Value{int64(1)}), // IS_ROLEMEMBER
			makeQueryResult([]string{"form"}, []driver.Value{"admin"}),
		})
		defer db.Close()

		posture, reasons, err := mssqlPostureOf(ctx, db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posture != rlsExempt {
			t.Errorf("posture = %v, want rlsExempt", posture)
		}
		if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "admin") {
			t.Errorf("reasons do not name the predicate form: %v", reasons)
		}
	})

	t.Run("member under the plain predicate is NOT exempt -- cleat#2760's exact bug", func(t *testing.T) {
		db := openMSSQLMockDB([]checkDBResult{
			makeQueryResult([]string{""}, []driver.Value{int64(1)}), // IS_ROLEMEMBER
			makeQueryResult([]string{"form"}, []driver.Value{"plain"}),
		})
		defer db.Close()

		posture, reasons, err := mssqlPostureOf(ctx, db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posture != rlsSubject {
			t.Errorf("posture = %v, want rlsSubject -- a member under the plain predicate "+
				"is admitted nothing, and reporting rlsExempt here is cleat#2760 itself", posture)
		}
		if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "plain") {
			t.Errorf("reasons do not explain why membership did not help: %v", reasons)
		}
	})

	t.Run("non-member is subject without a second query", func(t *testing.T) {
		db := openMSSQLMockDB([]checkDBResult{
			makeQueryResult([]string{""}, []driver.Value{int64(0)}), // IS_ROLEMEMBER only
		})
		defer db.Close()

		posture, _, err := mssqlPostureOf(ctx, db)
		if err != nil {
			t.Fatalf("unexpected error (a second, unscripted query would produce one): %v", err)
		}
		if posture != rlsSubject {
			t.Errorf("posture = %v, want rlsSubject", posture)
		}
	})

	t.Run("no admin role means unprotected, unchanged", func(t *testing.T) {
		db := openMSSQLMockDB([]checkDBResult{
			makeQueryResult([]string{""}, []driver.Value{nil}), // IS_ROLEMEMBER: NULL
		})
		defer db.Close()

		posture, reasons, err := mssqlPostureOf(ctx, db)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if posture != rlsUnprotected {
			t.Errorf("posture = %v, want rlsUnprotected", posture)
		}
		if len(reasons) == 0 || reasons[0].Kind != "no-admin-role" {
			t.Errorf("reasons = %v, want a no-admin-role reason", reasons)
		}
	})

	t.Run("a member whose predicate form cannot be read is UNKNOWN, not exempt", func(t *testing.T) {
		db := openMSSQLMockDB([]checkDBResult{
			makeQueryResult([]string{""}, []driver.Value{int64(1)}), // IS_ROLEMEMBER
			makeQueryError(errors.New("invalid object name 'admin.rls_predicate_form'")),
		})
		defer db.Close()

		posture, _, err := mssqlPostureOf(ctx, db)
		if err == nil {
			t.Fatal("expected an error when the predicate form cannot be read")
		}
		if posture != rlsUnknown {
			t.Errorf("posture = %v, want rlsUnknown -- an unreadable predicate form must not "+
				"default to exempt on the strength of membership alone", posture)
		}
	})
}

// TestRLSPostureOfClassifiesARealConnection is the part the stub cannot cover.
//
// Everything else in this file drives runCheckDB with rlsPostureFn replaced, so
// it tests what the command DOES with an answer. That leaves the derivation
// itself unmeasured, and the derivation is where the polarity is: the same
// engine.CheckRLSEnforced whose empty result means "good" for cleat-worker means
// "cleatctl cannot do its job" here.
//
// So this asks two real PostgreSQL connections to the same database, and the
// two must disagree. A test that only asserted the superuser case would pass
// against a function that always returns rlsExempt. cleat#1184.
func TestRLSPostureOfClassifiesARealConnection(t *testing.T) {
	superDB := testutil.TestDB(t, testutil.DialectPostgres)
	appDB := testutil.OpenPostgresRLSTestDB(t, superDB)

	ctx := context.Background()

	superPosture, superReasons, err := rlsPostureOf(ctx, superDB, "postgres")
	if err != nil {
		t.Fatalf("posture of the superuser connection: %v", err)
	}
	if superPosture != rlsExempt {
		t.Errorf("the superuser connection classified as %v, want rlsExempt.\n\n"+
			"reasons: %v\n\n"+
			"This is the connection cleatctl needs. If it does not read as exempt, every "+
			"cleatctl invocation on a correctly configured deployment prints the warning "+
			"telling the operator to switch to the credential they are already using.",
			superPosture, superReasons)
	}

	appPosture, _, err := rlsPostureOf(ctx, appDB, "postgres")
	if err != nil {
		t.Fatalf("posture of the RLS-subject connection: %v", err)
	}
	if appPosture != rlsSubject {
		t.Errorf("the RLS-subject connection classified as %v, want rlsSubject.\n\n"+
			"engine.CheckRLSEnforced returns the reasons row-level security would NOT "+
			"apply, so an EMPTY result is what says this connection is subject to it. "+
			"Reading that backwards is the one mistake this classification can make, and "+
			"it fails open: cleatctl would report cluster-wide answers it cannot see.",
			appPosture)
	}

	// The two must differ. Asserting each in isolation would pass against a
	// function that ignored its argument, which is the failure mode a pair of
	// separate tests cannot catch.
	if superPosture == appPosture {
		t.Errorf("both connections classified as %v: the posture does not depend on the "+
			"connection at all", superPosture)
	}
}

// TestCheckDBReportsAConnectionItCannotReadFrom is the regression test for the
// behaviour cleat#1184 describes: check-db said "STATUS: healthy" on a database
// whose core tables it could not read.
func TestCheckDBReportsAConnectionItCannotReadFrom(t *testing.T) {
	restore := rlsPostureFn
	t.Cleanup(func() { rlsPostureFn = restore })

	script := checkDBHealthyScript(fmt.Errorf(
		"pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped query"))

	rlsPostureFn = stubPosture(rlsSubject, nil)
	stdout, stderr := runCheckDBTestNoStub(t, script, nil)

	if strings.Contains(stdout, "STATUS: healthy") {
		t.Errorf("check-db reported healthy on a connection it could not read from.\n\n"+
			"stdout:\n%s", stdout)
	}
	for _, want := range []string{
		"RLS: connection IS subject to row-level security",
		"INSTANCES: UNREADABLE",
		"DEGRADED",
		"superuser or BYPASSRLS",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q.\n\nstderr:\n%s", want, stderr)
		}
	}
}

// TestCheckDBReportsADatabaseThatIsNotEnforcing covers the third posture, which
// is the one most easily folded into "exempt" by mistake: cleatctl's reads
// succeed, so nothing goes wrong for the operator running it -- and the reason
// they succeed is that the database is isolating nobody.
func TestCheckDBReportsADatabaseThatIsNotEnforcing(t *testing.T) {
	restore := rlsPostureFn
	t.Cleanup(func() { rlsPostureFn = restore })

	rlsPostureFn = stubPosture(rlsUnprotected, nil)
	stdout, stderr := runCheckDBTestNoStub(t, checkDBHealthyScript(nil), nil)

	if strings.Contains(stdout, "STATUS: healthy") {
		t.Errorf("a database with no row-level security in force is not healthy.\n\n"+
			"stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "not enforcing tenant isolation") {
		t.Errorf("stderr does not report the database as unprotected.\n\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no table in schema public has any policy") {
		t.Errorf("the reason is not reported, only the verdict.\n\nstderr:\n%s", stderr)
	}
}

// TestCheckDBSurvivesAnUndeterminablePosture: an inconclusive check must not be
// silent, and must not be fatal either. Same reasoning as cleat-worker's
// rErr != nil arm.
func TestCheckDBSurvivesAnUndeterminablePosture(t *testing.T) {
	restore := rlsPostureFn
	t.Cleanup(func() { rlsPostureFn = restore })

	rlsPostureFn = stubPosture(rlsUnknown, errors.New("catalogue unavailable"))
	_, stderr := runCheckDBTestNoStub(t, checkDBHealthyScript(nil), nil)

	if !strings.Contains(stderr, "cannot determine enforcement") {
		t.Errorf("an inconclusive check said nothing.\n\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "catalogue unavailable") {
		t.Errorf("the underlying error is not named.\n\nstderr:\n%s", stderr)
	}
}

// checkDBHealthyScript is the statement sequence runCheckDB issues on a working
// database. instanceErr, when non-nil, makes the workflow_instances read fail;
// the event_history reads then fail too, which is what a tenant-context error
// does in practice -- both tables carry the same policy.
func checkDBHealthyScript(instanceErr error) []checkDBResult {
	script := []checkDBResult{
		makePingResult(nil),
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"057", nil}),
	}
	for i := 0; i < 13; i++ {
		script = append(script, makeQueryResult([]string{"count"}, []driver.Value{int64(1)}))
	}
	if instanceErr != nil {
		script = append(script,
			makeQueryError(instanceErr), // instances
			makeQueryError(instanceErr), // event history size
			makeQueryError(instanceErr), // event history count fallback
			makeQueryError(instanceErr), // dead letters
		)
		return script
	}
	return append(script,
		makeQueryResult([]string{"status", "cnt"}, []driver.Value{"completed", int64(1)}),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
}
