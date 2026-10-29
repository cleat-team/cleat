package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3566. A run of a tenant other than the worker's own was reported lost by
// the worker's heartbeat at its FIRST beat, never had its heartbeat_at renewed,
// had its next durable call refused by the fence, and was released and re-claimed
// instead of finishing. Measured on develop and on the v0.5.0 tag with a tenant
// run that outlived a beat: the generation climbed 1 to 6 in about a minute and
// the run never reached a terminal state.
//
// # Why no existing suite saw it
//
// HeartbeatBatchFenced reads workflow_leases, which is FORCE ROW LEVEL SECURITY,
// and the tenant it sees is a property of the store instance. So the bug needs
// all three of: a role the policy applies to, a run of a tenant that is not the
// worker's, and a run that is still executing when a beat lands. The suites that
// run a real worker connect as a superuser, which bypasses the policy, so the
// heartbeat saw every row; the ones that run another tenant use a recording store,
// which can show a store was asked and not that it could see a row; and the one
// that ran this end to end (the integration-hub tenant sandbox scenario) ran
// guests that end within seconds, so it failed only when a claim landed just
// before a tick, once in thirty-one runs.
//
// # What this does, and why it is not phase-dependent
//
// A real guest (the defer fixture: two sequential durable calls) runs a tenant's
// run through a real worker as cleat_app. Inside the first call's HTTP handler,
// while the guest is still executing, the test forces THREE beats by calling
// heartbeatAndFenceInFlight itself, so the beats do not depend on when a ticker
// happens to fire relative to the claim. It then asserts, from a connection that
// bypasses the policy:
//
//   - heartbeat_at moved on every beat, which is the property the reaper rests on,
//     and is what "not fenced" alone would not show;
//   - the second durable call was made, which the fence refuses once a run is
//     marked lost;
//   - the run is done, not released.
//
// The same body runs on a superuser handle, where the old code passed too: that is
// the known-positive, the evidence that the difference is the role and not the
// fixture.
func TestATenantsRunOutlivesItsHeartbeatsUnderRLS(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed cleat#3566 test")
	}
	owner := testutil.SuiteTestDB(t, "cleat_worker_hbtenant")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)

	// PRECONDITION: the role this runs as neither bypasses the policy nor is a
	// superuser. Without it a pass says nothing about the heartbeat.
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}

	t.Run("as cleat_app, which the policy applies to", func(t *testing.T) {
		runATenantsRunThroughItsBeats(t, owner, rlsDB, "11111111-0000-4000-8000-000000003566", true)
	})
	t.Run("twin: as a superuser, which bypasses it (the old code passed here too)", func(t *testing.T) {
		runATenantsRunThroughItsBeats(t, owner, owner, "22222222-0000-4000-8000-000000003566", false)
	})
}

func runATenantsRunThroughItsBeats(t *testing.T, owner, db *sql.DB, tenant string, policyApplies bool) {
	t.Helper()
	ctx := context.Background()

	if _, err := owner.Exec(
		`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, false) ON CONFLICT (tenant_id) DO NOTHING`,
		tenant, "cleat-3566-"+tenant[:8]); err != nil {
		t.Fatalf("registering tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
		_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenant)
	})

	factory := engine.NewPostgresStoreFactory(db, "public")
	tenantStore, closeTenant, err := factory.OpenStore(ctx, tenant)
	if err != nil {
		t.Fatalf("opening the tenant's store: %v", err)
	}
	defer closeTenant.Close()

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "hb-tenant-3566"
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef under the tenant: %v", err)
	}
	wfID := fmt.Sprintf("hb-tenant-3566-%d", time.Now().UnixNano())
	if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun for the tenant: %v", err)
	}

	logs := &syncBuffer{}
	// The worker's OWN store is the default tenant's, as in a worker started with
	// no --tenant, and it reaches the tenant's run only through the factory.
	w := newRealDeferPhaseWorker(t, db, engine.NewPostgresStore(db), logs)
	w.id = "hb-tenant-3566-worker"
	w.storeFactory = factory
	w.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimed := claimOne(t, ctx, tenantStore, w.id, wfID)
	if claimed.TenantID != tenant {
		t.Fatalf("claimed run belongs to %q, want %q", claimed.TenantID, tenant)
	}
	w.inflight.Store(wfID, claimed)

	// PREMISE, measured, so a pass cannot be for the wrong reason: through a
	// connection scoped to the WORKER'S tenant the run's lease row is hidden when
	// the policy applies and visible when it does not; scoped to the run's own
	// tenant it is visible either way.
	if got := visibleLeaseRows(t, db, engine.DefaultTenantUUID, wfID); policyApplies && got != 0 || !policyApplies && got != 1 {
		t.Fatalf("scoped to the worker's tenant the run's lease row is visible %d time(s) (policy applies: %v); "+
			"the premise of this test does not hold on this database", got, policyApplies)
	}
	if got := visibleLeaseRows(t, db, tenant, wfID); got != 1 {
		t.Fatalf("scoped to the run's own tenant the lease row is visible %d time(s), want 1; "+
			"\"hidden by the policy\" cannot be told from \"not there\"", got)
	}

	heartbeatAt := func() time.Time {
		var at time.Time
		if err := owner.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&at); err != nil {
			t.Fatalf("reading heartbeat_at: %v", err)
		}
		return at
	}
	atClaim := heartbeatAt()

	calls := &recordedCalls{}
	var mu sync.Mutex
	beats := []time.Time{atClaim}
	var fencedAfter []bool
	var landed []bool
	svc := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		calls.add(op)
		if op == "work/first" {
			// The guest is executing right now. Force three beats, spaced so that
			// now() differs, instead of waiting for a ticker whose phase against
			// the claim is not ours to choose.
			for i := 0; i < 3; i++ {
				time.Sleep(20 * time.Millisecond)
				ok := w.heartbeatAndFenceInFlight()
				mu.Lock()
				landed = append(landed, ok)
				beats = append(beats, heartbeatAt())
				fencedAfter = append(fencedAfter, w.runIsFenced(wfID, claimed.Generation))
				mu.Unlock()
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"ok":true}`)
	}))
	defer svc.Close()
	old := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = old }()

	runExecuteWorkflow(w, claimed)

	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 4 {
		t.Fatalf("recorded %d heartbeat reading(s), want the claim plus 3 (the guest never reached its first call; logs:\n%s)",
			len(beats), logs.String())
	}
	for i := 1; i < len(beats); i++ {
		if !landed[i-1] {
			t.Errorf("beat %d did not land", i)
		}
		if fencedAfter[i-1] {
			t.Errorf("beat %d fenced a run that is healthy and held by this worker. The heartbeat asked a store whose "+
				"tenant is not the run's, so the run's lease row was invisible to it (cleat#3566)", i)
		}
		if !beats[i].After(beats[i-1]) {
			t.Errorf("heartbeat_at did not move on beat %d (%s then %s): the reaper reads this column, so a run whose "+
				"heartbeat is never renewed is reclaimable while it is still executing", i, beats[i-1], beats[i])
		}
	}
	if !calls.has("work/second") {
		t.Errorf("the guest never made its second durable call: a run marked lost has its next call refused (logs:\n%s)", logs.String())
	}
	if got := statusOf(t, ctx, tenantStore, wfID); got != "done" {
		t.Errorf("status after the run = %q, want \"done\": a fenced run is released and re-claimed, not finished (logs:\n%s)",
			got, logs.String())
	}
}

// visibleLeaseRows counts the run's lease row through a connection whose tenant
// context is set to tenant, as the store's own transactions set it.
func visibleLeaseRows(t *testing.T, db *sql.DB, tenant, wfID string) int {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT set_config('cleat.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM workflow_leases WHERE id = $1`, wfID).Scan(&n); err != nil {
		t.Fatalf("counting visible lease rows as tenant %s: %v", tenant, err)
	}
	return n
}
