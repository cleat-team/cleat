package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestARealPostgresDockerPauseDoesNotLetAnIdleWorkerReclaimALiveRun is the
// real-database reproduction cleat-review asked for, on top of the
// deterministic mock-based one in reaper_recovery_grace_period_test.go: two
// real Workers against a real PostgreSQL container, one busy and one
// genuinely idle, with the database made unreachable by `docker pause`
// (a real network stall -- calls hang rather than error, which is the shape
// dbCallDeadline exists for) rather than a simulated block.
//
// This is deliberately gated on a second env var, not just CLEAT_TEST_POSTGRES:
// pausing a container by name only makes sense against a container this
// process actually controls, and every other integration test in this
// package only needs the DSN. Point CLEAT_TEST_DOCKER_PAUSE_CONTAINER at the
// container backing CLEAT_TEST_POSTGRES; this test never guesses a name.
//
// Falsified by reverting BOTH reapingIsSafe (to its one-condition form from
// 35c641b5, checking only lastDBTrouble) and heartbeatAndFenceInFlight's idle
// branch (to return success with no DBPinger probe at all) -- i.e. the state
// this issue's very first fix left the idle-worker case in. Against that
// mutation this test must fail, with the busy worker's run reclaimed despite
// being alive throughout the pause. See the falsification note at the end of
// this file for the exact patch and how to apply/restore it.
func TestARealPostgresDockerPauseDoesNotLetAnIdleWorkerReclaimALiveRun(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed docker-pause reproduction")
	}
	container := os.Getenv("CLEAT_TEST_DOCKER_PAUSE_CONTAINER")
	if container == "" {
		t.Skip("CLEAT_TEST_DOCKER_PAUSE_CONTAINER not set -- this test pauses a real " +
			"container and needs to be told which one backs CLEAT_TEST_POSTGRES; " +
			"every other integration test in this package only needs the DSN")
	}
	if testing.Short() {
		t.Skip("Skipping a real docker-pause reproduction in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	db := testutil.SuiteTestDB(t, "cleat_worker")
	ctx := context.Background()
	store := engine.NewPostgresStore(db)

	const defName = "docker-pause-repro-def"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("docker-pause-repro-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1,
		json.RawMessage(`{}`), "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	const heartbeat = 2 * time.Second
	// reclaimWin is the SAME invariant reclaimWindow derives in production
	// (see minimumReclaimAfter), not a value picked for this test -- so a
	// change to that formula changes this test's timing automatically
	// instead of silently drifting out of sync with it, the way this test's
	// old hardcoded `4 * time.Second` (== heartbeat + 2*dbCallDeadline, the
	// PRE-round-3 formula) did the moment the invariant was raised.
	reclaimWin := minimumReclaimAfter(heartbeat)
	steadyState := reclaimWin + 2*time.Second   // > reclaimWin, so both workers start from a settled state
	pauseDuration := reclaimWin + 4*time.Second // > reclaimWin, so the row is genuinely stale by the raw predicate

	busy := newRealBackgroundLoopWorker(t, db, store, "docker-pause-busy")
	busy.heartbeatInterval = heartbeat
	busy.reclaimTimeout = reclaimWin
	idle := newRealBackgroundLoopWorker(t, db, store, "docker-pause-idle")
	idle.heartbeatInterval = heartbeat
	idle.reclaimTimeout = reclaimWin

	claimed := claimOne(t, ctx, store, busy.id, wfID)
	busy.inflight.Store(wfID, claimed)

	// Drive both workers with the REAL heartbeatLoop -- production cadence,
	// including its Timer-not-Ticker re-arm and its retry-sooner-after-a-
	// failed-call behaviour (see heartbeatLoop's and heartbeatRetryInterval's
	// doc comments) -- rather than an artificial fixed-interval poll. GAP2
	// from cleat#2005's review: a 250ms poll exercises a cadence no
	// production worker ever runs at, and cannot reproduce a defect (like
	// Edge 1's single-failed-retry sliver) that depends on the real timer's
	// behaviour around a failed call.
	//
	// Each loop gets its OWN per-loop context (initLoopCtxUnmonitored),
	// distinct from the worker's root w.ctx, so it can be stopped on its own
	// before idle.reapOnce() runs below -- cancelling w.ctx itself would also
	// cancel every context reapOnce derives from it (probeBoundedCall does
	// exactly that), turning the reap call into an instant context-cancelled
	// error rather than a real measurement.
	busy.initLoopCtxUnmonitored("heartbeat")
	idle.initLoopCtxUnmonitored("heartbeat")
	stopHeartbeatLoop := func(w *Worker) {
		w.loopMu.Lock()
		lc := w.loopCtxMap["heartbeat"]
		w.loopMu.Unlock()
		lc.cancel()
	}
	busy.wg.Add(1)
	go busy.heartbeatLoop()
	idle.wg.Add(1)
	go idle.heartbeatLoop()

	t.Logf("steady state for %s before pausing %s", steadyState, container)
	time.Sleep(steadyState)

	if out, err := exec.Command("docker", "pause", container).CombinedOutput(); err != nil {
		t.Fatalf("docker pause %s: %v\n%s", container, err, out)
	}
	t.Logf("paused %s for %s", container, pauseDuration)
	time.Sleep(pauseDuration)

	if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil {
		// Best-effort: try again before giving up, so a failed test run does
		// not also leave the container paused for whatever runs next against it.
		time.Sleep(time.Second)
		if out2, err2 := exec.Command("docker", "unpause", container).CombinedOutput(); err2 != nil {
			t.Fatalf("docker unpause %s: %v\n%s\n(retry: %v\n%s)", container, err, out, err2, out2)
		}
	}
	t.Logf("unpaused %s", container)

	// Isolate the mechanism: stop driving both loops, THEN clear the stall,
	// THEN fire the idle worker's reaper exactly once. A real reaperLoop
	// ticks at max(heartbeatInterval, 10s); calling reapOnce in a loop here
	// would let idle's OWN error-detecting reap calls (rather than its
	// heartbeat ping) account for the correct outcome, same trap the
	// mock-based two-worker test in reaper_recovery_grace_period_test.go
	// documents hitting first.
	stopHeartbeatLoop(busy)
	stopHeartbeatLoop(idle)
	busy.wg.Wait()
	idle.wg.Wait()

	idle.reapOnce()

	stored, err := store.GetWorkflowByID(context.Background(), wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatalf("workflow %s not found after the reap attempt", wfID)
	}
	if stored.Status != "running" || stored.AssignedTo != busy.id {
		t.Fatalf("an idle worker reclaimed a busy worker's live run across a real docker-pause "+
			"stall: status=%q assigned_to=%q (want running/%s)", stored.Status, stored.AssignedTo, busy.id)
	}
	t.Logf("run %s correctly still held by %s after the pause", wfID, busy.id)
}

// newRealBackgroundLoopWorker builds a Worker against a real store for
// exercising heartbeatAndFenceInFlight/reapOnce specifically -- no wasmtime
// backend, since nothing here calls executeWorkflow. Seeded pessimistically,
// matching production: a worker that has never made a call has proven
// nothing about its own database contact.
func newRealBackgroundLoopWorker(t *testing.T, db *sql.DB, store engine.WorkflowStore, id string) *Worker {
	t.Helper()
	workerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	monitor := NewMemoryMonitor(5 * time.Second)
	w := &Worker{
		Metrics:             newTestPrometheus(),
		id:                  id,
		store:               store,
		storeTenantID:       engine.DefaultTenantUUID,
		db:                  db,
		concurrency:         5,
		memoryController:    NewMemoryController(monitor, store, id, 5, 1<<40, 1<<40),
		compactionThreshold: engine.DefaultCompactionThreshold,
		compactionInterval:  time.Hour,
		ctx:                 workerCtx,
		cancel:              cancel,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		wasmCache:           newWasmLRUCache(100, 500),
		healthTracker:       newHealthTracker(),
		loopCtxMap:          make(map[string]*loopContext),
	}
	w.lastHeartbeatOK.Store(time.Now().UnixNano())
	w.lastDBTrouble.Store(time.Now().UnixNano())
	return w
}

// Falsification, applied by hand and reverted (never committed):
//
//   func (w *Worker) reapingIsSafe() bool {
//       last := time.Unix(0, w.lastDBTrouble.Load())
//       return time.Since(last) >= w.reclaimAfter()
//   }
//
// and heartbeatAndFenceInFlight's `if len(runs) == 0 { ... }` body replaced
// with:
//
//   w.lastHeartbeatOK.Store(time.Now().UnixNano())
//   w.Metrics.RecordBackgroundLoop(w.ctx, "heartbeat", "ok")
//   return true
//
// i.e. exactly cleat#2005's first fix (35c641b5), before the DBPinger review
// round. Against that pair, this test fails: the idle worker's gate opens
// purely from elapsed wall-clock time since its own construction, and it
// reclaims the busy worker's run immediately after the pause ends even
// though the busy worker was alive and heartbeating (unsuccessfully, but
// trying) throughout.
