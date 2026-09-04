package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// The coverage gap IMPROVEMENT-PLAN 3.112 stated rather than papered over.
//
// engine/defer_phase_vertical_test.go runs terminate -> claim -> segment ->
// finalize on a real guest and a real database, and its own comment says where
// it stops: "It stops one layer short of cmd/cleat-worker: the ~30 lines of
// executeWorkflow that read PendingTerminalStatus, add the option and call
// finishDeferPhase are not exercised here." cmd/cleat-worker/defer_phase_test.go
// covers those lines' effects with a mock store, but calls finishDeferPhase
// DIRECTLY -- so the branch that decides to call it is what neither test has.
//
// That branch is four lines, and deleting them is silent. A defer segment comes
// back SUSPENDED, because the body replays, asks for its first piece of new work
// and is refused. Without the branch that suspension reaches the ordinary
// reschedule path, which is a correct reading of a suspension and the wrong
// reading of this one: the workflow goes back to 'terminating', is claimed
// again, suspends again, forever. Nothing errors and nothing is logged as
// wrong. A terminate simply never completes.
//
// So this test drives w.executeWorkflow TWICE against the same workflow, with a
// real WASM guest, a real PostgreSQL, and the worker's own service caller:
//
//	segment 1  ordinary work -- registers a defer, sleeps, suspends
//	terminate  marks 'terminating' and records the outcome
//	segment 2  the defer phase -- the branch under test
//
// and asserts both that the cleanup reached the outside world and that the
// finalize happened. The note above the final assertions says why neither half
// is redundant, and which half the falsification actually turned red.
func TestTheWorkerRunsADeferPhaseRatherThanReschedulingIt(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed defer phase test")
	}
	ctx := context.Background()

	// The defer body's only observable effect is a DurableCall, which is what
	// the host can see (testdata/deferfunc's package comment). Pointing the
	// worker's own dbServiceCaller at a test server makes that call SUCCEED
	// and makes it visible here -- without one it returns "service not
	// configured", and a cleanup that was refused looks the same from the
	// database as a cleanup that ran.
	calls := &recordedCalls{}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// dbServiceCaller.forwardToBenchSvc posts to /call/<service>/<operation>.
		calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	store := engine.NewPostgresStore(db)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	const defName = "defer-phase-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("defer-phase-worker-%d", time.Now().UnixNano())
	input := json.RawMessage(`{"__entry_point":"defer_survives_suspension"}`)
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)

	// ---- Segment 1: ordinary work. Registers the defer, sleeps, suspends.
	runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, wfID))

	// The worker records a sleeping workflow as `ready` with next_wake_at set,
	// NOT as `suspended` -- the same finalized-`ready` shape 3.83 is about. Both
	// are claimable and both owe a defer phase, so this accepts either rather
	// than pinning the one this happened to produce; what it must not be is
	// terminal, which would mean the sleep never happened.
	if got := statusOf(t, ctx, store, wfID); got != "ready" && got != "suspended" {
		t.Fatalf("status after segment 1 = %q, want \"ready\" or \"suspended\"; the fixture "+
			"no longer reaches the state this test terminates from (logs:\n%s)", got, logs.String())
	}
	if !strings.Contains(logs.String(), "workflow suspended") {
		t.Fatalf("segment 1 did not suspend on the fixture's sleep, so nothing is left "+
			"pending for a defer phase to run (logs:\n%s)", logs.String())
	}
	if calls.has("cleanup/after_sleep") {
		t.Fatal("the cleanup ran during segment 1; a suspension is not workflow exit, " +
			"so there is no pending defer left for the defer phase to run and the " +
			"rest of this test would measure nothing")
	}
	if !historyHasDefer(t, ctx, store, wfID) {
		t.Fatal("segment 1 recorded no defer event, so TerminateWorkflow will take " +
			"the ONE-phase path and no defer phase will exist to test")
	}

	// ---- Phase 1 of the terminal transition.
	if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
	if got := statusOf(t, ctx, store, wfID); got != "terminating" {
		t.Fatalf("status after terminate = %q, want \"terminating\"", got)
	}

	// ---- Segment 2: the defer phase. The dispatch loop's ordinary claim is
	// what starts it, which is the whole point of leaving the row schedulable.
	claimed := claimOne(t, ctx, store, w.id, wfID)
	if claimed.PendingTerminalStatus != "terminated" {
		t.Fatalf("the claim carried PendingTerminalStatus %q, want %q; executeWorkflow "+
			"reads exactly this field to decide, so the branch under test could not fire",
			claimed.PendingTerminalStatus, "terminated")
	}
	runExecuteWorkflow(w, claimed)

	// Four assertions, and they catch different things -- which is worth
	// spelling out, because the obvious reading of the first one is wrong.
	//
	// MEASURED 2026-09-04 by deleting the branch: the segment still ran the
	// drain. The log said `ran a defer segment's defers defers_run=1` and this
	// service still saw cleanup/after_sleep, because WithDeferPhase is set
	// before Replay and the branch under test runs AFTER it. So the cleanup
	// call is NOT what catches a missing branch, and a test resting on it
	// alone would have passed over the deletion.
	//
	// What the cleanup call does catch is the other way this run could reach a
	// terminal row: the `if err != nil { if deferPhase { ... } }` arm ABOVE
	// this one in executeWorkflow, which finalizes a defer segment that
	// FAILED. It applies the same recorded outcome and logs "defer phase
	// failed; terminating without its cleanup" -- a terminal row with the
	// cleanup lost, which is exactly what 3.112 exists to prevent. Asserting
	// only the status passes on both. That one is read from the code rather
	// than measured here: this fixture has no way to make its own segment
	// fail, so the assertion below stands as a guard on a path a future change
	// could route this run down, not as a reproduction of one.
	//
	// The two together are the test: the cleanup proves the segment did its
	// job, and the finalize log proves the branch that decides what to do with
	// a finished segment is the one that ran. With the branch deleted the
	// finalize simply never happens -- the run logged "workflow suspended" a
	// second time and the row went back to 'terminating', which is the
	// reschedules-itself-forever failure in this test's opening comment,
	// observed rather than predicted.
	if !calls.has("cleanup/after_sleep") {
		t.Fatalf("the defer phase did not run the registered cleanup; the service saw %v.\n"+
			"Terminate destroyed this workflow's cleanup instead of running it (logs:\n%s)",
			calls.all(), logs.String())
	}
	if strings.Contains(logs.String(), "defer phase failed") {
		t.Fatalf("the defer segment errored and was finalized by the failure arm, not by "+
			"the post-segment branch this test is for (logs:\n%s)", logs.String())
	}
	if !strings.Contains(logs.String(), "defer phase complete; terminal outcome applied") {
		t.Fatalf("finishDeferPhase never reported success, so the branch that calls it did "+
			"not run to completion (logs:\n%s)", logs.String())
	}

	// And then the row, which is what a reschedule would get wrong: without
	// the branch, ReleaseWorkflow puts a marked workflow back to 'terminating'
	// and it is claimed and suspended again forever.
	if got := statusOf(t, ctx, store, wfID); got != "terminated" {
		t.Fatalf("status after the defer phase = %q, want \"terminated\"", got)
	}
	var pending sql.NullString
	var deadline sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT pending_terminal_status, defer_phase_deadline FROM workflow_instances WHERE id = $1`,
		wfID).Scan(&pending, &deadline); err != nil {
		t.Fatalf("reading the marker back: %v", err)
	}
	if pending.Valid || deadline.Valid {
		t.Errorf("marker survived the finalize: pending_terminal_status=%v defer_phase_deadline=%v; "+
			"ExpireDeferPhases would re-apply an outcome over a terminal row", pending, deadline)
	}
}

// recordedCalls is what the defer body's DurableCall reached.
type recordedCalls struct {
	mu  sync.Mutex
	ops []string
}

func (r *recordedCalls) add(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

func (r *recordedCalls) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func (r *recordedCalls) has(op string) bool {
	for _, got := range r.all() {
		if got == op {
			return true
		}
	}
	return false
}

// syncBuffer is a bytes.Buffer safe for the logger to write from the goroutines
// executeWorkflow starts.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newRealDeferPhaseWorker is a Worker with everything executeWorkflow reaches on
// the path this test takes: a real store, a real database handle and the real
// wasmtime backend.
//
// The execution limits come from the flag DEFAULTS rather than from zero
// values. A zero wasmInstanceTimeout is documented as "disables it and is NOT
// recommended", so a worker built from zeroes would run the guest under limits
// no deployment uses, and this test is about what a deployment does.
func newRealDeferPhaseWorker(t *testing.T, db *sql.DB, store engine.WorkflowStore, logs *syncBuffer) *Worker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	backend, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { backend.Close(ctx) })

	monitor := NewMemoryMonitor(5 * time.Second)
	return &Worker{
		Metrics:              newTestPrometheus(),
		id:                   "defer-phase-worker",
		store:                store,
		storeTenantID:        engine.DefaultTenantUUID,
		db:                   db,
		wasmtimeBackend:      backend,
		concurrency:          1,
		memoryController:     NewMemoryController(monitor, store, "defer-phase-worker", 1, 1<<40, 1<<40),
		heartbeatInterval:    10 * time.Millisecond,
		pollInterval:         1 * time.Millisecond,
		compactionThreshold:  engine.DefaultCompactionThreshold,
		compactionInterval:   time.Hour,
		ctx:                  ctx,
		cancel:               cancel,
		logger:               slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		wasmCache:            newWasmLRUCache(100, 500),
		healthTracker:        newHealthTracker(),
		loopCtxMap:           make(map[string]*loopContext),
		maxRetries:           *maxRetries,
		wasmInstanceTimeout:  *wasmInstanceTimeout,
		wasmWallClockCeiling: *wasmWallClockCeiling,
		hostRetryBudget:      *hostRetryBudget,
	}
}

// claimOne claims through the ordinary dispatch path -- which is the point,
// since "the dispatch loop can still see a marked workflow" is half of what the
// two-phase transition rests on -- and returns the one workflow this test is
// about.
//
// It RELEASES everything else it claimed. ci.yml's `commands` matrix entry runs
// ./cmd/... without -p 1, so cmd/cleatctl's database tests share this
// PostgreSQL, and a claim is not a read: holding another package's workflow for
// the length of this test would make its failure look like a flake in that
// package. Releasing is exact rather than best-effort -- these are rows nothing
// else had claimed a moment ago, and they go back with their own generation and
// wake time.
func claimOne(t *testing.T, ctx context.Context, store engine.WorkflowStore, workerID, wfID string) *engine.WorkflowInstance {
	t.Helper()
	claimed, err := store.ClaimWorkflows(ctx, workerID, 50)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	var mine *engine.WorkflowInstance
	for _, wf := range claimed {
		if wf.ID == wfID {
			mine = wf
			continue
		}
		if err := store.ReleaseWorkflow(ctx, wf.ID, workerID, wf.Generation, wf.NextWakeAt); err != nil {
			t.Logf("releasing %s, which this test claimed but does not own: %v", wf.ID, err)
		}
	}
	if mine == nil {
		t.Fatalf("workflow %s was not claimed; got %d workflow(s). A workflow the claim "+
			"cannot see is a defer phase that never runs", wfID, len(claimed))
	}
	return mine
}

func statusOf(t *testing.T, ctx context.Context, store engine.WorkflowStore, wfID string) string {
	t.Helper()
	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatalf("workflow %s not found", wfID)
	}
	return wf.Status
}

func historyHasDefer(t *testing.T, ctx context.Context, store engine.WorkflowStore, wfID string) bool {
	t.Helper()
	history, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	for _, e := range history {
		if e.EventType == engine.EventTypeDefer {
			return true
		}
	}
	return false
}

// buildDeferFixture compiles testdata/deferfunc the way engine's own tests do
// (buildFixtureWasm in engine/host_test.go), which is the same way `cleat build`
// produces a deployed module.
func buildDeferFixture(t *testing.T) []byte {
	t.Helper()
	// No testing.Short() guard of its own: every caller has already been
	// through testutil.TestDB, which skips in short mode. A second skip here
	// would be one more conditional skip in the baseline that can never fire.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd)) // cmd/cleat-worker -> repo root

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, "testdata", "deferfunc"))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm file found in %s", tmpDir)
	return nil
}
