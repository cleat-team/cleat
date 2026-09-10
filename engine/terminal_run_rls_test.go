package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// GetTerminalRun on PostgreSQL, over a real chain, against a connection that
// cannot bypass row-level security. cleat#1177.
//
// WHY THIS DID NOT EXIST. Every other GetTerminalRun test drives a mockStore or
// a stubWorkflowStore, so PostgresStore.successorOfRun -- the statement with the
// defect -- had never executed against a database in any test in this repo.
// What it lacked was BOTH the transaction and the tenant predicate: it ran on
// the plain *sql.DB, and workflow_instances carries a fail-closed policy,
// `USING (tenant_id = cleat.assert_tenant_set())`, whose function RAISEs when
// the setting is missing. The tenant is set with `set_config(..., true)` --
// is_local -- so it exists only inside beginTxWithRLS.
//
// TWO THINGS HAVE TO BE TRUE FOR THIS TEST TO MEAN ANYTHING, and each is a way
// the obvious version of it passes while measuring nothing:
//
//  1. The connection must be subject to RLS. testutil.TestDB hands back a
//     superuser, which PostgreSQL never applies a policy to -- against that
//     connection the unfixed statement succeeds and this test is green on
//     broken code. Hence OpenPostgresRLSTestDB, which is neither superuser nor
//     table owner.
//
//  2. There must be a row for the policy to evaluate. A USING expression runs
//     PER ROW, so against an empty table assert_tenant_set is never called and
//     the unfixed statement returns ("", nil) with no error -- the correct
//     answer for a run that never continued. The chain below is seeded first
//     and the assertion is that the successor is FOUND, not that no error
//     occurred.
//
// The second one is why nobody hit this in production either: the failure needs
// a successor to exist, which is the only case the function is for.
func TestGetTerminalRunFollowsARealChainUnderRLS(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const (
		tenantA = "d1d1d1d1-d1d1-4d1d-d1d1-d1d1d1d1d1d1"
		tenantB = "d2d2d2d2-d2d2-4d2d-d2d2-d2d2d2d2d2d2"
		defName = "rls-terminal-chain"
	)

	storeA := NewPostgresStore(appDB).WithTenant(tenantA)
	if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	headID := fmt.Sprintf("rls-terminal-head-%d", time.Now().UnixNano())
	if _, _, err := storeA.StartNewRun(ctx, headID, defName, 1,
		json.RawMessage(`{"remaining":1}`), "", tenantA, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	wf, err := storeA.ClaimWorkflow(ctx, "worker-1")
	if err != nil || wf == nil {
		t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
	}
	successorID, err := storeA.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation,
		defName, 1, json.RawMessage(`{"remaining":0}`), nil, "", nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	if successorID == wf.ID {
		t.Fatalf("ContinueAsNew returned the id it was given (%s); there is no chain to walk "+
			"and every assertion below would be vacuous", successorID)
	}

	// The head of a two-run chain must resolve to its successor.
	//
	// Against the unfixed statement this is not a wrong answer, it is an error:
	//   ERROR: cleat.tenant_id is not set
	// raised by the policy the moment it has a candidate row to evaluate.
	got, err := storeA.GetTerminalRun(ctx, headID)
	if err != nil {
		t.Fatalf("GetTerminalRun(%s): %v\n\n"+
			"A continue-as-new chain is unfollowable if this errors. If the message is "+
			"\"cleat.tenant_id is not set\", successorOfRun is issuing its SELECT outside "+
			"beginTxWithRLS: the tenant is established with set_config(..., true), which is "+
			"transaction-local, so there is no setting for the policy to read (cleat#1177).",
			headID, err)
	}
	if got == nil {
		t.Fatalf("GetTerminalRun(%s) returned no run at all", headID)
	}
	if got.ID != successorID {
		t.Errorf("GetTerminalRun(%s) resolved to %s, want the successor %s",
			headID, got.ID, successorID)
	}

	// CONTROL 1: another tenant, on the RLS-subject connection.
	//
	// This one is weaker than it looks, and the comment says so because I first
	// wrote that it "decides which repair was made" and then measured it.
	// Dropping the `AND tenant_id = $2` predicate while keeping the transaction
	// leaves this GREEN -- correctly, because inside beginTxWithRLS the policy
	// itself scopes the read, so tenant B cannot see tenant A's row whether or
	// not the statement says so. A control that cannot disagree with the thing
	// it is offered as evidence for is not evidence.
	//
	// What it does establish is real and worth keeping: the fix did not widen
	// the statement to every tenant on the connection deployments use.
	storeB := NewPostgresStore(appDB).WithTenant(tenantB)
	other, err := storeB.GetTerminalRun(ctx, headID)
	if err == nil && other != nil {
		t.Errorf("a store scoped to tenant B resolved tenant A's chain to %s under RLS.\n\n"+
			"The successor lookup is reaching rows outside its own tenant.", other.ID)
	}

	// CONTROL 2, which is the one that separates the two halves of the fix.
	//
	// The same cross-tenant question asked over a connection that BYPASSES RLS,
	// so the policy contributes nothing and the statement's own predicate is the
	// only thing scoping the read. Drop `AND tenant_id = $2` and this fails
	// while control 1 stays green.
	//
	// Not a hypothetical configuration. `-rls-check` accepts "off", and "auto"
	// only WARNS unless --require-auth is set, so a single-tenant deployment can
	// run with the policy unenforced -- and there the predicate is the whole of
	// the tenant scoping. The engine's own test harness is such a connection,
	// which is why the sibling assertions above had to be moved off it.
	// It asks successorOfRun DIRECTLY rather than through GetTerminalRun, and
	// that narrowing is not tidiness. GetTerminalRun begins with
	// GetWorkflowByID, whose PostgreSQL SELECT is `WHERE id = $1` with no
	// tenant predicate of its own -- it relies wholly on the policy. So over a
	// bypassing connection the walk returns tenant A's HEAD before it ever
	// reaches a successor, and a control written at that level fails on a
	// correctly fixed tree. Measured: it did, naming the head id.
	//
	// That is a real observation about GetWorkflowByID and it is reported
	// separately rather than folded in here. What this control must isolate is
	// the statement cleat#1177 is about.
	adminStoreB := NewPostgresStore(adminDB).WithTenant(tenantB)
	bypassed, err := adminStoreB.successorOfRun(ctx, wf.ID)
	if err != nil {
		t.Fatalf("successorOfRun over the bypassing connection: %v", err)
	}
	if bypassed != "" {
		t.Errorf("over a connection that bypasses RLS, successorOfRun scoped to tenant B "+
			"returned tenant A's successor %s.\n\n"+
			"With the policy contributing nothing, `AND tenant_id = $2` is the only thing "+
			"keeping this read inside its tenant -- and it is not there. That configuration "+
			"is reachable: -rls-check accepts \"off\", and the default \"auto\" only WARNS "+
			"unless --require-auth is set (cleat#1177).", bypassed)
	}
}
