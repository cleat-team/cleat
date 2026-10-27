package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 3: Heartbeat and HeartbeatBatchFenced now
// mirror their heartbeat_at write onto workflow_leases, on the same tx as the
// workflow_instances write.
//
// This worktree does not carry piece 1 or piece 2 as uncommitted code (both
// already merged to develop, via #3298/#3299), but this test still seeds
// workflow_leases directly via SQL rather than through ClaimWorkflow -- same
// independent-testability shape the two merged pieces established, kept here
// so this test does not depend on claim's own dual-write being correct.
func heartbeatLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})
	return NewPostgresStore(db), db
}

// seedClaimedInstanceAndLeaseRow seeds a run through testutil.SeedRun, already
// claimed by workerID at the given generation, with an hour-stale heartbeat_at
// and started_at on its lease. defName must be a deployed definition.
func seedClaimedInstanceAndLeaseRow(t *testing.T, db *sql.DB, id, defName, workerID string, generation int64) {
	t.Helper()
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID: id, TenantID: DefaultTenantUUID, DefName: defName,
		Status: "running", AssignedTo: workerID, Generation: generation,
	})
	staleAt := time.Now().Add(-1 * time.Hour).Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = $2, started_at = $2 WHERE id = $1`, id, staleAt); err != nil {
		t.Fatalf("make %s's lease heartbeat stale: %v", id, err)
	}
}

func readLeaseHeartbeatAt(t *testing.T, db *sql.DB, id string) time.Time {
	t.Helper()
	var hb time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, id).Scan(&hb); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at for %s: %v", id, err)
	}
	return hb
}

func TestHeartbeatUpdatesTheLeaseRow(t *testing.T) {
	store, db := heartbeatLeaseTestStore(t)
	ctx := context.Background()

	const defName = "heartbeat-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "heartbeat-lease-wf-1"
	const worker = "worker-heartbeat-1"
	seedClaimedInstanceAndLeaseRow(t, db, id, defName, worker, 1)

	// Precondition: the lease starts with a stale heartbeat_at, an hour in the
	// past -- otherwise a no-op Heartbeat could coincidentally read back as
	// "already current".
	preLease := readLeaseHeartbeatAt(t, db, id)
	if time.Since(preLease) < 30*time.Minute {
		t.Fatalf("UNMEASURED: precondition not met -- seeded heartbeat_at %v is not an hour stale", preLease)
	}

	ok, err := store.Heartbeat(ctx, id, worker, 1)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !ok {
		t.Fatal("Heartbeat returned false, want true (worker/generation match)")
	}

	postLease := readLeaseHeartbeatAt(t, db, id)
	if !postLease.After(preLease) {
		t.Errorf("workflow_leases.heartbeat_at did not advance: pre=%v post=%v", preLease, postLease)
	}
}

func TestHeartbeatWithWrongGenerationLeavesTheLeaseRowUntouched(t *testing.T) {
	store, db := heartbeatLeaseTestStore(t)
	ctx := context.Background()

	const defName = "heartbeat-lease-fence-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "heartbeat-lease-fence-wf-1"
	const worker = "worker-heartbeat-fence-1"
	seedClaimedInstanceAndLeaseRow(t, db, id, defName, worker, 1)
	preLease := readLeaseHeartbeatAt(t, db, id)

	// A stale generation must be fenced identically on both tables -- i.e.
	// must not touch workflow_leases either.
	ok, err := store.Heartbeat(ctx, id, worker, 2)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if ok {
		t.Fatal("Heartbeat with a stale generation returned true, want false")
	}

	postLease := readLeaseHeartbeatAt(t, db, id)
	if !postLease.Equal(preLease) {
		t.Errorf("workflow_leases.heartbeat_at changed after a fenced-out heartbeat: pre=%v post=%v", preLease, postLease)
	}
}

func TestHeartbeatBatchFencedUpdatesTheLeaseRows(t *testing.T) {
	store, db := heartbeatLeaseTestStore(t)
	ctx := context.Background()

	const defName = "heartbeat-batch-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const worker = "worker-heartbeat-batch-1"
	const liveID = "heartbeat-batch-lease-live"
	const staleID = "heartbeat-batch-lease-stale-gen"
	seedClaimedInstanceAndLeaseRow(t, db, liveID, defName, worker, 1)
	// staleID is claimed by the same worker but at a generation the batch
	// call will NOT present, so it must land in "lost" and its lease row
	// must not advance.
	seedClaimedInstanceAndLeaseRow(t, db, staleID, defName, worker, 1)

	preLiveLease := readLeaseHeartbeatAt(t, db, liveID)
	preStaleLease := readLeaseHeartbeatAt(t, db, staleID)

	lost, err := store.HeartbeatBatchFenced(ctx, worker, []GenerationKey{
		{WorkflowID: liveID, Generation: 1},
		{WorkflowID: staleID, Generation: 2}, // does not match the seeded generation 1
	})
	if err != nil {
		t.Fatalf("HeartbeatBatchFenced: %v", err)
	}
	if len(lost) != 1 || lost[0] != staleID {
		t.Fatalf("HeartbeatBatchFenced lost = %v, want exactly [%q]", lost, staleID)
	}

	postLiveLease := readLeaseHeartbeatAt(t, db, liveID)
	if !postLiveLease.After(preLiveLease) {
		t.Errorf("workflow_leases.heartbeat_at for the live id did not advance: pre=%v post=%v", preLiveLease, postLiveLease)
	}

	postStaleLease := readLeaseHeartbeatAt(t, db, staleID)
	if !postStaleLease.Equal(preStaleLease) {
		t.Errorf("workflow_leases.heartbeat_at for the lost id changed: pre=%v post=%v", preStaleLease, postStaleLease)
	}
}
