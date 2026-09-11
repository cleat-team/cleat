package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

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

	superPosture, superReasons, err := rlsPostureOf(ctx, superDB)
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

	appPosture, _, err := rlsPostureOf(ctx, appDB)
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
