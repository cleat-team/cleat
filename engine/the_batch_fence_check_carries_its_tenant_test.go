package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The adaptive flusher's batch fence check runs under row-level security.
// cleat#1677.
//
// WHAT WAS WRONG. partitionFencedBatch issued its UPDATE on af.db -- the pool,
// no transaction, so no set_config('cleat.tenant_id', ...). workflow_instances
// is ENABLE + FORCE ROW LEVEL SECURITY with
// USING (tenant_id = cleat.assert_tenant_set()), and that function RAISES when
// the tenant is unset rather than filtering to nothing. So the statement did not
// return the wrong rows: it could not run at all.
//
// THE ROLE IS THE WHOLE TEST. testutil.TestDB's connection is a superuser, and
// PostgreSQL exempts superusers from RLS unconditionally -- a version of this
// test written against it passes whether the fix is present or not, which is
// the failure mode that let this defect exist behind a guard.
// testutil.OpenPostgresRLSTestDB is neither superuser nor table owner, which is
// the shape cleat_app has in production.
//
// WHY THE FIX IS A TRANSACTION RATHER THAN A CTE. The flusher's two other
// writes establish the tenant with a
// `WITH cfg AS (SELECT set_config(...))` CTE and they work, so matching the
// neighbours was the first attempt. It does not carry an UPDATE. Measured
// against the NOSUPERUSER app role, both ways:
//
//	cfg declared, never referenced   -> P0001, an unreferenced CTE is not evaluated
//	cfg referenced via FROM ..., cfg -> P0001 ANYWAY
//
// The policy on the UPDATE's TARGET is evaluated before the CTE's set_config
// has taken effect. So the idiom that carries an INSERT does not carry this,
// and the fix is an explicit BeginTx -> set_config -> statement -> Commit.
//
// The Commit is load bearing: this statement also renews heartbeat_at, so
// without it the lease refresh rolls back with the transaction.
func TestTheBatchFenceCheckCarriesItsTenant(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenant = "d1677000-1677-4677-8677-d16770001677"
	store := NewPostgresStore(appDB).WithTenant(tenant)

	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "fence-tenant", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("fence-tenant-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, "fence-tenant", 1,
		json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := store.ClaimWorkflow(ctx, "worker-1677")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v (nil=%v)", err, claimed == nil)
	}

	// THE PRECONDITION, checked rather than assumed. If this connection does
	// not actually enforce RLS then every assertion below passes for the wrong
	// reason -- which is precisely how a superuser-based version of this test
	// would have read. An unscoped read of an RLS table must RAISE here.
	var n int
	probeErr := appDB.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances`).Scan(&n)
	if probeErr == nil {
		t.Fatalf("an unscoped read of workflow_instances succeeded on the app connection, so "+
			"this connection does not enforce RLS and nothing below is measuring the fix.\n"+
			"count returned %d", n)
	}
	if !strings.Contains(probeErr.Error(), "tenant_id is not set") {
		t.Fatalf("the unscoped read failed for an unexpected reason: %v\n\n"+
			"Expected cleat.assert_tenant_set to raise. If the policy changed, this test's "+
			"precondition needs rewriting rather than deleting", probeErr)
	}

	af := &AdaptiveFlusher{db: appDB, tenantID: tenant}
	batch := []batchEntry{
		{workflowID: wfID, workerID: "worker-1677", generation: claimed.Generation},
	}

	held, lost, err := af.partitionFencedBatch(ctx, batch)
	if err != nil {
		t.Fatalf("partitionFencedBatch under RLS: %v\n\n"+
			"This is cleat#1677: the fence check ran on the pool with no tenant, and the "+
			"policy's assert_tenant_set() raises rather than filtering -- so the statement "+
			"could not execute at all, not merely return the wrong rows.", err)
	}
	if len(held) != 1 || held[0].workflowID != wfID {
		t.Errorf("held = %+v, want exactly the claimed entry (%s)", held, wfID)
	}
	if len(lost) != 0 {
		t.Errorf("lost = %+v, want none: the claim is still held by worker-1677", lost)
	}

	// The control for the assertion above. Without it, "held == 1" is also
	// satisfied by a fence check that fences nothing and passes everything
	// through -- which is what the function does when no entry asks to be
	// fenced, and would look identical here.
	stale := []batchEntry{
		{workflowID: wfID, workerID: "worker-1677", generation: claimed.Generation + 99},
	}
	held2, lost2, err := af.partitionFencedBatch(ctx, stale)
	if err != nil {
		t.Fatalf("partitionFencedBatch (stale generation): %v", err)
	}
	if len(lost2) != 1 || len(held2) != 0 {
		t.Errorf("a stale generation gave held=%+v lost=%+v, want it reported LOST.\n\n"+
			"The fence is not actually comparing the generation, so the success above says "+
			"nothing about fencing", held2, lost2)
	}
}
