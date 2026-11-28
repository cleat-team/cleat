package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3570. ReapStaleInstances carries no tenant predicate, so what it reclaims
// is whatever row-level security (PostgreSQL) or the security policy (SQL Server)
// admits for the store it runs through. The reaper ran through the worker's own
// store only, so a crashed worker's run of any other tenant was invisible to
// every survivor.
//
// Measured end to end with two real workers as cleat_app and a SIGKILL: a run of
// the survivor's own tenant was reclaimed about 50 s after the kill; a run of a
// non-default tenant stayed `running` on the dead worker, generation 1,
// reclaim_count 0, heartbeat_at frozen at the kill, for the whole 304 s it was
// watched, and the survivor's log never mentioned it.
//
// # This is that scenario reduced to a test
//
// A holder "crashes" by claiming the run and then going silent: it never beats
// again, which is all a SIGKILL leaves behind. A survivor with its own tenant
// the default one, as a worker started with no --tenant is, runs reapOnce. The
// run must be reclaimed (reclaim_count 1), re-claimed by the survivor, and
// executed to `done`.
//
// PostgreSQL runs it as cleat_app, which the policy applies to, and as a
// superuser, which bypasses it: the superuser twin is the known-positive, since
// the old code reclaimed there too, so the difference is the role and not the
// fixture. SQL Server has no bypassing principal, so its premise probe plays that
// part. MySQL is not applicable: tenancy there is a database per tenant and it is
// single-tenant by decision.
func TestACrashedTenantsRunIsReclaimedUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_reaptenant")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	t.Run("postgres as cleat_app, which the policy applies to", func(t *testing.T) {
		runACrashedTenantsRunIsReclaimed(t, postgresHBRig(t, owner, rlsDB, true, "44444444-0000-4000-8000-000000003570"))
	})
	t.Run("postgres twin: as a superuser, which bypasses it (the old code passed here too)", func(t *testing.T) {
		runACrashedTenantsRunIsReclaimed(t, postgresHBRig(t, owner, owner, false, "55555555-0000-4000-8000-000000003570"))
	})
}

func TestACrashedTenantsRunIsReclaimedUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3570 test")
	}
	runACrashedTenantsRunIsReclaimed(t, mssqlHBRig(t, "66666666-0000-4000-8000-000000003570"))
}

func runACrashedTenantsRunIsReclaimed(t *testing.T, rig *hbRig) {
	t.Helper()
	ctx := context.Background()
	tenant := rig.tenant
	rig.register(t, tenant)

	tenantStore, closeTenant, err := rig.factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("opening the tenant's store: %v", err)
	}
	defer closeTenant.Close()

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "reap-tenant-3570"
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef under the tenant: %v", err)
	}
	wfID := fmt.Sprintf("reap-tenant-3570-%d", time.Now().UnixNano())
	if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun for the tenant: %v", err)
	}

	// THE CRASH: a holder claims the run and is never heard from again.
	const crashed = "crashed-holder-3570"
	held := claimOne(t, ctx, tenantStore, crashed, wfID)
	if held.TenantID != tenant {
		t.Fatalf("claimed run belongs to %q, want %q", held.TenantID, tenant)
	}

	// The survivor: its OWN store is the default tenant's, and it reaches the
	// tenant's run only through the factory.
	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, rig.db, rig.ownStore, logs)
	w.id = "reap-tenant-3570-survivor"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true
	w.maxReclaimPerTick = 100
	w.reclaimTimeout = time.Second
	w.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}
	seedRecentlyConfirmedHealthy(w)

	// PREMISE, measured, so a pass cannot be for the wrong reason: through a
	// connection scoped to the SURVIVOR's tenant the run's lease state is hidden
	// when the policy applies and visible when it does not.
	if got := rig.visibleRows(t, engine.DefaultTenantUUID, wfID); rig.policyApplies && got != 0 || !rig.policyApplies && got != 1 {
		t.Fatalf("%s: scoped to the survivor's tenant the run's lease state is visible %d time(s) (policy applies: %v); "+
			"the premise of this test does not hold on this database", rig.name, got, rig.policyApplies)
	}
	if got := rig.visibleRows(t, tenant, wfID); got != 1 {
		t.Fatalf("%s: scoped to the run's own tenant the lease state is visible %d time(s), want 1", rig.name, got)
	}
	if wf, err := tenantStore.GetWorkflowByID(ctx, wfID); err != nil || wf.Status != "running" || wf.ReclaimCount != 0 {
		t.Fatalf("before the survivor reaps: want running with reclaim_count 0, got %+v (err %v)", wf, err)
	}

	// The survivor reaps until the run is reclaimed or the budget is spent. The
	// budget is the only clock here: reaping before the heartbeat is stale
	// reclaims nothing, and nothing is asserted about when.
	deadline := time.Now().Add(20 * time.Second)
	var reclaimed bool
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		w.reapOnce()
		if wf, err := tenantStore.GetWorkflowByID(ctx, wfID); err == nil && wf.ReclaimCount >= 1 {
			reclaimed = true
			break
		}
	}
	if !reclaimed {
		wf, _ := tenantStore.GetWorkflowByID(ctx, wfID)
		t.Fatalf("a run of a non-default tenant, held by a holder that went silent, was never reclaimed by a surviving worker "+
			"(state %+v). The reaper reached only the survivor's own tenant, so the run stays running on a dead worker forever (cleat#3570); logs:\n%s",
			wf, logs.String())
	}

	// And it is a run again, not a row that merely changed: the survivor claims it
	// and the guest finishes it.
	calls := &recordedCalls{}
	svc := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"ok":true}`)
	}))
	defer svc.Close()
	old := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = old }()

	var mine *engine.WorkflowInstance
	for i := 0; i < 50 && mine == nil; i++ {
		claimedBy, err := tenantStore.ClaimWorkflows(ctx, w.id, 50)
		if err != nil {
			t.Fatalf("ClaimWorkflows: %v", err)
		}
		for _, wf := range claimedBy {
			if wf.ID == wfID {
				mine = wf
			} else {
				_ = tenantStore.ReleaseWorkflow(ctx, wf.ID, w.id, wf.Generation, wf.NextWakeAt)
			}
		}
		if mine == nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if mine == nil {
		t.Fatalf("the reclaimed run was never claimable by the survivor (logs:\n%s)", logs.String())
	}
	w.inflight.Store(wfID, mine)
	// This worker runs no heartbeat loop, so once its reclaim window has passed the
	// guest's first durable call is refused as "heartbeat presumed lost"
	// (cleat#2008): a fact about the harness, not the reaper. The window was 1s so
	// the reap above would see the crashed holder as stale quickly, and a cold
	// wasmtime compile on a slow runner outlasts it (CI failed this test, and so did
	// one of cleat-review's runs). It is not widened to a guess: the reap is done, so
	// the window is now an hour and no timing of the guest can reach it.
	w.reclaimTimeout = time.Hour
	w.lastHeartbeatOK.Store(time.Now().UnixNano())
	runExecuteWorkflow(w, mine)

	if !calls.has("work/second") {
		t.Errorf("the reclaimed run never completed its durable calls (logs:\n%s)", logs.String())
	}
	if got := statusOf(t, ctx, tenantStore, wfID); got != "done" {
		t.Errorf("status after the survivor ran it = %q, want \"done\" (logs:\n%s)", got, logs.String())
	}
}
