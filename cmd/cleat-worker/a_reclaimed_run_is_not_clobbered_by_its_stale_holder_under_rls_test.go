package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3566 question 3: can a second worker execute a run while its first holder is
// still executing it, and what does the stale holder do next? Measured on PostgreSQL as
// cleat_app with a non-default tenant, two real Workers, B's REAL reaper reclaiming across
// tenants, and a real wasmtime guest (TwoSequentialCalls: work.first, then work.second).
//
// Holder A claims the run and blocks inside call 1 with no heartbeat loop running. The
// lease is aged past the reclaim window, B's reaper reclaims it, B claims it and runs it to
// the end, and only then is A released. Two workers have then executed the same run. What
// stops A is a stack of layers, and each variant isolates one:
//
//   - the holder believes it is healthy and the call is a default (at-least-once) op:
//     nothing refuses the dispatch. A can send one more external call; what it cannot do is
//     change the run. Its finalize is fenced and B's outcome stands. This variant pins
//     those durable invariants and cleat#2008's "at most twice" bound, and deliberately does
//     NOT assert that a duplicate happens: that is a limit, not a behaviour to keep.
//   - the holder's own heartbeat belief is older than the reclaim window, which is what a
//     real stall does (cleat#2008 decision 2): it refuses to start new work.
//   - the call is a write-ahead-intent op: the intent write is fenced and precedes dispatch,
//     so a holder that lost the run cannot dispatch it even while it believes it is healthy.
//
// The refusal REASON is asserted in the last two, not only that no call arrived: with the
// layer under test removed, another layer could still stop the call, and a status-only
// check would then pass for the wrong reason.
func TestAReclaimedRunIsNotClobberedByItsStaleHolderUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_stalehold")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("role attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this would measure nothing", bypass, super)
	}
	t.Run("a default op, holder believes it is healthy: only the fenced finalize stops it, and the run stays the successor's", func(t *testing.T) {
		got := runAStaleHolder(t, owner, rlsDB, "cccccccc-0000-4000-8000-000000003566", false, "")
		// cleat#2008: a call's key reaches the service at most twice (the original and a
		// second worker's replay). work.first was in flight when the lease was reclaimed.
		for op, n := range got.counts {
			if n > 2 {
				t.Errorf("%s reached the service %d times, want at most 2: %v", op, n, got.counts)
			}
		}
		if !strings.Contains(got.logsA, "finalize: fence lost") {
			t.Errorf("the holder's finalize was not refused by the fence (its log has no \"finalize: fence lost\"):\n%s", got.logsA)
		}
	})
	t.Run("a default op, holder's own heartbeat belief is older than the reclaim window: it refuses new work", func(t *testing.T) {
		got := runAStaleHolder(t, owner, rlsDB, "dddddddd-0000-4000-8000-000000003566", true, "")
		if len(got.afterRelease) != 0 {
			t.Errorf("the stalled holder made further external calls after its lease was reclaimed: %v (cleat#2008 decision 2 should refuse them)", got.afterRelease)
		}
		if !strings.Contains(got.errA, "worker heartbeat presumed lost") {
			t.Errorf("the holder was not stopped by its own heartbeat presumption; its error was:\n%s", got.errA)
		}
	})
	t.Run("a write-ahead-intent op, holder believes it is healthy: the fenced intent write refuses the dispatch", func(t *testing.T) {
		got := runAStaleHolder(t, owner, rlsDB, "eeeeeeee-0000-4000-8000-000000003566", false, "work.second")
		if len(got.afterRelease) != 0 {
			t.Errorf("a write-ahead-intent call was dispatched by a holder that no longer held the run: %v", got.afterRelease)
		}
		if !strings.Contains(got.errA, "write-ahead intent for work.second") || !strings.Contains(got.errA, "fence lost") {
			t.Errorf("the dispatch was not refused by the fenced intent write; the holder's error was:\n%s", got.errA)
		}
	})
}

type staleHolderResult struct {
	counts       map[string]int
	afterRelease []string
	logsA        string
	errA         string // the holder's "execution error" log line, if any
}

func runAStaleHolder(t *testing.T, owner, rlsDB *sql.DB, tenant string, ageBelief bool, waiOps string) staleHolderResult {
	t.Helper()
	rig := postgresHBRig(t, owner, rlsDB, true, tenant)
	ctx := context.Background()
	rig.register(t, tenant)

	if waiOps != "" {
		old := *writeAheadIntentOps
		*writeAheadIntentOps = waiOps
		defer func() { *writeAheadIntentOps = old }()
	}

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
	const defName = "stale-holder-3566"
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("stale-holder-3566-%d", time.Now().UnixNano())
	if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID) })

	type arrival struct {
		op    string
		after bool // arrived after A was released
	}
	var (
		mu        sync.Mutex
		arrivals  []arrival
		released  atomic.Bool
		heldFirst atomic.Bool
		aInCall   = make(chan struct{})
		releaseA  = make(chan struct{})
		releaseMu sync.Once
	)
	release := func() { releaseMu.Do(func() { released.Store(true); close(releaseA) }) }

	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		mu.Lock()
		arrivals = append(arrivals, arrival{op: op, after: released.Load()})
		mu.Unlock()
		// Only the FIRST arrival of work/first (A's) is held. A sync.Once would be wrong
		// here: every other caller of Do blocks until the first returns, which would hold
		// B's request behind A's and make B time out.
		if op == "work/first" && heldFirst.CompareAndSwap(false, true) {
			close(aInCall)
			<-releaseA
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	// Deferred AFTER svc.Close, so it runs BEFORE it: a failing test must not leave A's
	// request held, because Server.Close waits for outstanding requests forever.
	defer release()
	oldURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldURL }()

	mkWorker := func(id string, logs *syncBuffer) *Worker {
		w := newRealDeferPhaseWorker(t, rig.db, rig.ownStore, logs)
		w.id = id
		w.storeTenantID = engine.DefaultTenantUUID
		w.storeFactory = rig.factory
		w.claimAcrossTenants = true
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		return w
	}
	leaseRow := func() string {
		var st, assigned string
		var gen, reclaims int64
		err := owner.QueryRow(`SELECT status, COALESCE(assigned_to,'<null>'), generation, reclaim_count FROM workflow_leases WHERE id = $1`, wfID).Scan(&st, &assigned, &gen, &reclaims)
		if err != nil {
			return "lease row: " + err.Error()
		}
		return fmt.Sprintf("status=%s assigned_to=%s generation=%d reclaim_count=%d", st, assigned, gen, reclaims)
	}

	logsA, logsB := &syncBuffer{}, &syncBuffer{}
	A := mkWorker("stale-holder-a", logsA)
	claimedA := claimOne(t, ctx, tenantStore, A.id, wfID)
	A.inflight.Store(wfID, claimedA)
	aDone := make(chan struct{})
	go func() { defer close(aDone); runExecuteWorkflow(A, claimedA) }()

	select {
	case <-aInCall:
	case <-time.After(60 * time.Second):
		t.Fatalf("A never reached its first call (logs:\n%s)", logsA.String())
	}
	// PREMISE: A holds the run, at generation 1, as the tenant's own lease row says.
	if row := leaseRow(); !strings.Contains(row, "status=running") || !strings.Contains(row, "assigned_to=stale-holder-a") || !strings.Contains(row, "generation=1") {
		t.Fatalf("A does not hold the run before the reclaim: %s", row)
	}

	// The lease goes stale: no heartbeat has renewed it for ten minutes.
	if _, err := owner.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, wfID); err != nil {
		t.Fatalf("ageing the lease: %v", err)
	}

	B := mkWorker("stale-holder-b", logsB)
	B.maxReclaimPerTick = 100
	B.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(B)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(leaseRow(), "reclaim_count=1") {
		B.reapOnce()
		time.Sleep(200 * time.Millisecond)
	}
	if row := leaseRow(); !strings.Contains(row, "reclaim_count=1") || !strings.Contains(row, "generation=2") {
		t.Fatalf("B's reaper never reclaimed the run across tenants: %s (B's logs:\n%s)", row, logsB.String())
	}

	// B's reclaim window was 1s so its reaper would see A's lease as stale at once. The same
	// window is how long B will vouch for its OWN heartbeat (heartbeatPresumedLostFor), and B
	// runs no heartbeat loop, so left at 1s B refuses to start work as soon as a second has
	// passed since it was built: it passed alone and failed in the full package, where a cold
	// wasmtime compile outlasts it. The reap is done, so the window is now an hour and no
	// timing of the guest can reach it.
	B.reclaimTimeout = time.Hour
	claimedB := claimOne(t, ctx, tenantStore, B.id, wfID)
	B.inflight.Store(wfID, claimedB)
	bDone := make(chan struct{})
	go func() { defer close(bDone); runExecuteWorkflow(B, claimedB) }()
	select {
	case <-bDone:
	case <-time.After(90 * time.Second):
		t.Fatalf("B did not finish within 90s (logs:\n%s)", logsB.String())
	}
	if got := statusOf(t, ctx, tenantStore, wfID); got != "done" {
		t.Fatalf("B did not complete the reclaimed run: status %q (B's logs:\n%s)", got, logsB.String())
	}
	afterB := leaseRow()

	if ageBelief {
		// What a genuine stall does to A: its heartbeats have not landed for longer than
		// the reclaim window, so its own record of the last good one is that old.
		old := time.Now().Add(-10 * time.Minute).UnixNano()
		A.lastHeartbeatOK.Store(old)
		A.groupHeartbeatOK.Store(A.heartbeatGroupFor(tenant), old)
		if !A.heartbeatPresumedLostFor(tenant) {
			t.Fatalf("ageing A's heartbeat belief did not make it presume the tenant's runs lost; the premise does not hold")
		}
	} else if A.heartbeatPresumedLostFor(tenant) {
		t.Fatalf("A already presumes the tenant's runs lost; this variant needs it to believe it is healthy")
	}

	release()
	select {
	case <-aDone:
	case <-time.After(60 * time.Second):
		t.Fatalf("A's execution did not return within 60s of its release (logs:\n%s)", logsA.String())
	}
	time.Sleep(300 * time.Millisecond) // a late request from A would arrive here

	// A cannot change the run: the lease row is exactly what B left, and B's outcome stands.
	if got := leaseRow(); got != afterB {
		t.Errorf("the stale holder changed the lease row: before its release %q, after %q", afterB, got)
	}
	if got := statusOf(t, ctx, tenantStore, wfID); got != "done" {
		t.Errorf("the stale holder changed the run's status: %q, want done", got)
	}

	res := staleHolderResult{counts: map[string]int{}, logsA: logsA.String()}
	mu.Lock()
	for _, a := range arrivals {
		res.counts[a.op]++
		if a.after {
			res.afterRelease = append(res.afterRelease, a.op)
		}
	}
	mu.Unlock()
	for _, l := range strings.Split(res.logsA, "\n") {
		if strings.Contains(l, `msg="execution error"`) {
			res.errA = l
		}
	}
	return res
}
