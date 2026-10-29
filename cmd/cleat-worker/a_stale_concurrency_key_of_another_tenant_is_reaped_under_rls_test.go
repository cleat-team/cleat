package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3572. A run's concurrency key is released after the commit that makes the
// run terminal, and that release is best-effort by design
// (releaseWorkflowResources): a database blip right after the commit, or a worker
// dying between the two, leaves a key held by a finished run. The claim does not
// treat that row as free -- the new run is picked as a candidate, the insert
// conflicts with the stale row, and the holder is not the new run -- so the new
// run is refused on every poll until the key is reaped or expires, seven days on.
//
// The reaper scopes itself to its store's tenant (an explicit tenant_id predicate,
// and row-level security besides) and concurrencyKeyReaperLoop ran it through the
// worker's own store, so only the worker's own tenant's stale keys were ever
// freed. Measured as cleat_app: a stale key of a non-default tenant refused that
// tenant's new run for as long as it was watched, while the default tenant's was
// freed by the same reaper.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one, as for a worker started with no
// --tenant. A run of the other tenant holds a key and goes terminal without
// releasing it (the owner connection writes the status, which is all a failed
// release leaves behind). A second run under the same key is refused. The survivor
// runs one tick of the key reaper; the key must go and the second run must claim.
//
// The worker's own tenant, with the same steps, is the known-positive: the old code
// frees it, so the difference between the two subtests is whose key it is and
// nothing else. The cleat_app connection is asserted not to bypass the policy.
func TestAStaleConcurrencyKeyOfAnotherTenantIsReapedUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_ckreap")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	t.Run("a non-default tenant's stale key, reaped by a worker whose own tenant is the default", func(t *testing.T) {
		runAStaleKeyIsReaped(t, owner, rlsDB, "77777777-0000-4000-8000-000000003572")
	})
	t.Run("the worker's own tenant's stale key (the control: the old code frees this one too)", func(t *testing.T) {
		runAStaleKeyIsReaped(t, owner, rlsDB, engine.DefaultTenantUUID)
	})
}

func runAStaleKeyIsReaped(t *testing.T, owner, rlsDB *sql.DB, tenant string) {
	t.Helper()
	ctx := context.Background()
	rig := postgresHBRig(t, owner, rlsDB, true, tenant)
	if tenant != engine.DefaultTenantUUID {
		rig.register(t, tenant)
	}
	store, closeStore, err := rig.factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("opening the tenant's store: %v", err)
	}
	defer closeStore.Close()

	// The suite database outlives a run, and a run leaves its second run claimed and
	// holding the key, which would make the next run's first claim fail for a reason
	// that has nothing to do with the reaper. Clear this test's rows before and
	// after, by their id prefix and the tenant's keys.
	clear := func() {
		_, _ = owner.Exec(`DELETE FROM concurrency_keys WHERE tenant_id = $1`, tenant)
		_, _ = owner.Exec(`DELETE FROM workflow_leases WHERE id LIKE 'key-reap-3572-%' AND tenant_id = $1`, tenant)
		_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'key-reap-3572-%' AND tenant_id = $1`, tenant)
	}
	clear()
	t.Cleanup(clear)

	defName := "key-reap-3572-" + tenant[:8]
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	starter, ok := store.(interface {
		StartNewRunWithConcurrencyKey(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, concurrencyKey string) (string, bool, error)
	})
	if !ok {
		t.Fatalf("%T cannot start a run with a concurrency key", store)
	}
	const key = "key-reap-3572"
	start := func(label string) string {
		t.Helper()
		id := fmt.Sprintf("key-reap-3572-%s-%s-%d", label, tenant[:8], time.Now().UnixNano())
		if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx, id, defName, 1, json.RawMessage(`{}`), "", tenant, 0, key); err != nil {
			t.Fatalf("starting %s: %v", label, err)
		}
		return id
	}
	keyRows := func() int {
		var n int
		if err := owner.QueryRow(`SELECT count(*) FROM concurrency_keys WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
			t.Fatalf("counting key rows: %v", err)
		}
		return n
	}
	// claimsOf claims for up to n polls and reports whether wfID was among them,
	// handing back anything else it took, so nothing is left held.
	claimed := func(wfID string, polls int) bool {
		t.Helper()
		for i := 0; i < polls; i++ {
			got, err := store.ClaimWorkflows(ctx, "key-reap-claimer", 50)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			found := false
			for _, wf := range got {
				if wf.ID == wfID {
					found = true
					continue
				}
				_ = store.ReleaseWorkflow(ctx, wf.ID, "key-reap-claimer", wf.Generation, wf.NextWakeAt)
			}
			if found {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	// R1 takes the key by being claimed, then goes terminal WITHOUT releasing it.
	first := start("first")
	claimOne(t, ctx, store, "key-reap-holder", first)
	if keyRows() != 1 {
		t.Fatalf("the claimed run holds %d key row(s), want 1: the premise of this test does not hold", keyRows())
	}
	// The lease row carries a run's status; workflow_instances has none.
	if _, err := owner.Exec(`UPDATE workflow_leases SET status = 'failed' WHERE id = $1`, first); err != nil {
		t.Fatalf("ending the holder without releasing its key: %v", err)
	}
	if keyRows() != 1 {
		t.Fatalf("ending the holder released its key (%d rows); the stale state this test needs was not made", keyRows())
	}

	// PREMISE, measured: with the key held by a terminal run the next run is refused.
	second := start("second")
	if claimed(second, 3) {
		t.Fatalf("%s: a run under a key held by a TERMINAL run was claimed before any reaping; the premise does not hold", tenant)
	}

	// The survivor: its own store is the default tenant's, and it reaches the other
	// tenant only through the factory.
	w := newTestWorkerFromStore(rig.ownStore)
	defer w.cancel()
	w.id = "key-reap-survivor"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true
	w.reapConcurrencyKeysOnce()

	if got := keyRows(); got != 0 {
		t.Fatalf("a stale concurrency key of tenant %s survived one tick of a worker's key reaper (%d row(s)). The reaper "+
			"scopes itself to its store's tenant and ran through the worker's own store only (cleat#3572)", tenant, got)
	}
	if !claimed(second, 20) {
		t.Errorf("the key is gone but the tenant's next run under it was never claimable")
	}
}
