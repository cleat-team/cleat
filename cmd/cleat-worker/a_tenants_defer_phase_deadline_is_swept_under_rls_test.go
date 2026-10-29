package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3572's defer-phase arm. A workflow that owes cleanup is terminated in two
// phases: it enters 'terminating', and a worker replays it to run its defers. If
// the cleanup cannot finish, the sweep applies the recorded outcome once the
// deadline passes. That sweep, ExpireDeferPhases, is scoped to its store's tenant,
// and expireDeferPhases ran it through the worker's own store only, so a
// non-default tenant's terminating run past its deadline was never ended by a
// worker whose own tenant was the default.
//
// PostgreSQL runs it as cleat_app, which the policy applies to. SQL Server has no
// bypassing principal. MySQL is single-tenant by decision.
func TestATenantsDeferPhaseDeadlineIsSweptByAnotherTenantsWorkerUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_deferexpire")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	runADeferPhaseDeadlineIsSwept(t, postgresHBRig(t, owner, rlsDB, true, "aaaaaaaa-0000-4000-8000-000000003572"))
}

func TestATenantsDeferPhaseDeadlineIsSweptByAnotherTenantsWorkerUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3572 defer-phase test")
	}
	runADeferPhaseDeadlineIsSwept(t, mssqlHBRig(t, "bbbbbbbb-0000-4000-8000-000000003572"))
}

func runADeferPhaseDeadlineIsSwept(t *testing.T, rig *hbRig) {
	t.Helper()
	ctx := context.Background()
	other := rig.tenant
	rig.register(t, other)

	// terminating starts a run that owes a defer phase and terminates it, leaving it
	// 'terminating' with a deadline a defer-phase timeout in the future. It is ended
	// again on cleanup so a failing assertion cannot leave a claimable run behind in
	// a database the suite shares.
	terminating := func(tenant, label string) (engine.WorkflowStore, string) {
		t.Helper()
		store, closer, err := rig.factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("opening the store of %s: %v", tenant, err)
		}
		t.Cleanup(func() { _ = closer.Close() })
		const defName = "defer-expire-3572"
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef for %s: %v", tenant, err)
		}
		id := fmt.Sprintf("defer-expire-3572-%s-%s-%d", label, tenant[:8], time.Now().UnixNano())
		if _, _, err := store.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun for %s: %v", tenant, err)
		}
		if err := store.AppendEventHistoryBatch(ctx, id, []engine.EventRecord{
			{Step: 0, EventType: engine.EventTypeDefer, DeferID: "defer-0", DeferDescription: "release the lock"},
		}); err != nil {
			t.Fatalf("registering a defer for %s: %v", tenant, err)
		}
		if err := store.TerminateWorkflow(ctx, id, "cleat#3572"); err != nil {
			t.Fatalf("TerminateWorkflow for %s: %v", tenant, err)
		}
		t.Cleanup(func() { _ = store.TerminateWorkflow(context.Background(), id, "cleanup") })
		return store, id
	}
	status := func(st engine.WorkflowStore, id string) string {
		t.Helper()
		wf, err := st.GetWorkflowByID(ctx, id)
		if err != nil || wf == nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		return wf.Status
	}

	ownStore, ownID := terminating(engine.DefaultTenantUUID, "own")
	otherStore, expiredID := terminating(other, "expired")
	_, freshID := terminating(other, "fresh")
	rig.expireDeferDeadline(t, engine.DefaultTenantUUID, ownID)
	rig.expireDeferDeadline(t, other, expiredID)
	// freshID keeps its deadline: a sweep that ended every terminating run would pass
	// the assertions on the other two for the wrong reason.

	// PREMISE, measured: all three are 'terminating' before the sweep, and scoped to
	// the worker's own tenant the other tenant's lease state is hidden.
	for _, c := range []struct {
		st engine.WorkflowStore
		id string
	}{{ownStore, ownID}, {otherStore, expiredID}, {otherStore, freshID}} {
		if got := status(c.st, c.id); got != "terminating" {
			t.Fatalf("before the sweep %s is %q, want terminating: the premise of this test does not hold", c.id, got)
		}
	}
	if got := rig.visibleRows(t, engine.DefaultTenantUUID, expiredID); got != 0 {
		t.Fatalf("%s: scoped to the worker's own tenant the other tenant's run is visible %d time(s); the premise does not hold", rig.name, got)
	}

	// The worker's own store is the default tenant's; it reaches the other tenant only
	// through the factory.
	w := newTestWorkerFromStore(rig.ownStore)
	defer w.cancel()
	w.id = "defer-expire-3572-sweeper"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true

	w.expireDeferPhases()

	// KNOWN-POSITIVE: the worker's own tenant's expired phase ends. The old code did
	// this too, so a failure here is the fixture and not the change.
	if got := status(ownStore, ownID); got == "terminating" {
		t.Fatalf("the worker's own tenant's expired defer phase was not applied (still %q): the sweep itself is not working, "+
			"so the assertion below would say nothing", got)
	}
	// THE FIX: another tenant's expired phase ends too.
	if got := status(otherStore, expiredID); got == "terminating" {
		t.Fatalf("a non-default tenant's defer phase past its deadline was not applied by a worker whose own tenant is the "+
			"default (still %q), so the run stays 'terminating' until a worker of its own tenant happens to sweep (cleat#3572)", got)
	}
	// And only what is due: a phase still inside its deadline is untouched.
	if got := status(otherStore, freshID); got != "terminating" {
		t.Fatalf("a defer phase still inside its deadline was ended by the sweep (now %q)", got)
	}
}
