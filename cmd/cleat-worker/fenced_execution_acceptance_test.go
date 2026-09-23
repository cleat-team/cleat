package main

import (
	"context"
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

// TestAFencedExecutionMakesNoFurtherCallsAndASecondWorkerFinishesTheWorkflow
// is cleat#2008's own acceptance test, driven end to end: a real PostgreSQL, a
// real wasmtime guest, and two real Workers -- not the mocked heartbeat loop
// (cmd/cleat-worker/heartbeat_fenced_execution_test.go) or the mocked engine
// call site (engine/can_start_new_work_test.go), both of which prove their
// one mechanism in isolation. This proves the two mechanisms actually meet:
// that cancelling execCtx from inside a live Replay call really does stop the
// NEXT host call a real guest makes, not just a synthetic one.
//
// The fixture (testdata/deferfunc's TwoSequentialCalls) makes two plain
// DurableCalls, "first" then "second", with nothing suspending between them.
// The fence itself happens SYNCHRONOUSLY inside "first"'s own HTTP handler --
// bump the row's generation (simulating a reclaim by a second worker that
// ReapStaleInstances already elected) and call worker A's own
// heartbeatAndFenceInFlight() directly, in place of waiting on its ticker.
// That is what makes this deterministic rather than a timing race: by the
// time the guest asks for "second", A's execCtx is already cancelled.
//
// Acceptance criteria, from the issue: after a reclaim, execution A issues no
// further calls; the service sees each call's key at most twice (the
// original plus a second worker's replay); the fence loss is logged.
func TestAFencedExecutionMakesNoFurtherCallsAndASecondWorkerFinishesTheWorkflow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed fence acceptance test")
	}
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	const defName = "fence-acceptance-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("fence-acceptance-%d", time.Now().UnixNano())
	input := json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`)
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	calls := &recordedCalls{}
	var fenceOnce sync.Once
	var workerA *Worker // assigned below; the handler closes over the pointer, not a copy
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		calls.add(op)
		if op == "work/first" {
			// The reclaim: another worker's ReapStaleInstances already
			// reclaimed this row. Applied directly, scoped to this one row,
			// rather than by racing a real second claim or calling the real
			// sweep (which would be free to reap any OTHER test's in-flight
			// row on this shared database) -- same shape as
			// PostgresStore.ReapStaleInstances in engine/store_lifecycle.go,
			// so it puts the row back exactly where that sweep would.
			fenceOnce.Do(func() {
				if _, err := db.ExecContext(ctx, `
					UPDATE workflow_instances
					SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
					    generation = generation + 1, reclaim_count = reclaim_count + 1
					WHERE id = $1`, wfID); err != nil {
					t.Errorf("simulating the reclaim: %v", err)
					return
				}
				workerA.heartbeatAndFenceInFlight()
			})
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	logsA := &syncBuffer{}
	workerA = newRealDeferPhaseWorker(t, db, store, logsA)
	workerA.id = "fence-acceptance-worker-a"
	workerA.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimedA := claimOne(t, ctx, store, workerA.id, wfID)
	// Mimics the dispatch loop's own bookkeeping (setup.go's claim loop
	// stores into w.inflight before starting the goroutine); this test calls
	// executeWorkflow directly, so nothing else would populate it, and
	// heartbeatAndFenceInFlight reads only w.inflight to know what to ask
	// HeartbeatBatchFenced about.
	workerA.inflight.Store(wfID, claimedA)
	runExecuteWorkflow(workerA, claimedA)

	// ---- Acceptance criterion 1: execution A issues no further calls.
	if calls.has("work/second") {
		t.Fatalf("worker A called \"second\" after its fence was lost; the fenced execution "+
			"issued further work instead of stopping (logs:\n%s)", logsA.String())
	}
	if !calls.has("work/first") {
		t.Fatalf("\"first\" was never called, so the fence never had a chance to fire and this "+
			"test measured nothing (logs:\n%s)", logsA.String())
	}

	// ---- Acceptance criterion 3: the fence loss is logged.
	if !strings.Contains(logsA.String(), "fenced") && !strings.Contains(logsA.String(), "lost") {
		t.Fatalf("no log line recorded the fence loss (logs:\n%s)", logsA.String())
	}

	// A second worker claims the now-available row and finishes the workflow.
	// Its replay of "first" comes from recorded history, not a new call --
	// only "second" is new work for it.
	logsB := &syncBuffer{}
	workerB := newRealDeferPhaseWorker(t, db, store, logsB)
	workerB.id = "fence-acceptance-worker-b"
	workerB.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimedB := claimOne(t, ctx, store, workerB.id, wfID)
	workerB.inflight.Store(wfID, claimedB)
	runExecuteWorkflow(workerB, claimedB)

	if !calls.has("work/second") {
		t.Fatalf("the second worker never made the \"second\" call, so the workflow was not "+
			"actually recovered (logs:\n%s)", logsB.String())
	}
	if got := statusOf(t, ctx, store, wfID); got != "done" {
		t.Fatalf("status after the second worker's run = %q, want \"done\" (logs:\n%s)",
			got, logsB.String())
	}

	// ---- Acceptance criterion 2: the service sees each call's key at most
	// twice. "first" reached the service once, from A; the replay is not a
	// new call by design (it is answered from recorded history), so the
	// count staying at one here is the expected shape of THIS scenario, not
	// a looser bound this test is failing to exercise -- the "at most twice"
	// allowance in the issue covers a call still in flight at the moment of
	// the fence, which is a narrower race this deterministic test does not
	// attempt to reproduce.
	first := 0
	second := 0
	for _, op := range calls.all() {
		switch op {
		case "work/first":
			first++
		case "work/second":
			second++
		}
	}
	if first > 2 {
		t.Errorf("\"first\" reached the service %d times, want at most 2", first)
	}
	if second > 2 {
		t.Errorf("\"second\" reached the service %d times, want at most 2", second)
	}
}
