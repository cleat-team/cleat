package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4162, PR 2c: what does a store-level terminate or cancel do to a run that is HELD on a worker?
//
// Residency off, a run that is sleeping is a row in the store. A terminate settles that row and nothing is left to act: when the sleep's time comes there is no
// claim to win, so the guest never runs again. Residency on, the run is also a live instance in this worker's memory with a timer, and the store does not know
// that. The worker learns that a run was terminated through its heartbeat, which sees a lease whose generation moved (preemptivelySettle bumps it and clears
// assigned_to), and the heartbeat runs on an interval. If the sleep's timer fires inside that interval the held guest wakes up and runs, which a suspended one
// could not have done.
//
// This test holds the heartbeat back past the timer on purpose (the harness runs no heartbeat loop; heartbeatAndFenceInFlight is called by hand), which makes the
// worst case of that interval deterministic and not a race. The question it answers is one: after the terminate has committed, does the guest reach the call
// that follows its sleep?

// buildResidentSleepFixture builds testdata/residentsleep the way the fence acceptance test builds its fixture.
func buildResidentSleepFixture(t *testing.T) []byte {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	root := filepath.Dir(filepath.Dir(cwd)) // cmd/cleat-worker -> repo root
	out := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"), "build", "--target", "go", "-o", out, filepath.Join(root, "testdata", "residentsleep"))
	cmd.Dir = root
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(b), err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wasm") {
			b, err := os.ReadFile(filepath.Join(out, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm in the build output")
	return nil
}

type heldRunOutcome struct {
	logs              string
	generationAtClaim int64
	generationAtEnd   int64
	statusAtSettle    string        // the run's status when it was settled ("held" when it was in the resident set)
	heldAtTerminate   bool          // the run was asleep (held in the resident set, or a suspended row) when it was settled
	reachedTheCall    bool          // the guest reached the call that follows its sleep
	finalStatus       string        // the run's status in the store when everything had stopped
	timeToEviction    time.Duration // from the settle to the held run's execution stopping (0 when it was never held)
}

// runTerminatedWhileSleeping runs SleepLongThenCall (an 8 s durable sleep, then one call) with residency on or off, settles the run in the store while it sleeps with
// settle, and reports what the guest did. The heartbeat is NOT run until the guest has stopped, so a held run cannot learn of the settle from it.
// heartbeatFailingStore is the worker's store with a heartbeat call that can be made to fail, which is the one call a held run's claim check makes.
type heartbeatFailingStore struct {
	engine.WorkflowStore
	fail atomic.Bool
}

func (s *heartbeatFailingStore) HeartbeatBatchFenced(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
	if s.fail.Load() {
		return nil, errors.New("the heartbeat store is unreachable (injected by the test)")
	}
	return s.WorkflowStore.HeartbeatBatchFenced(ctx, workerID, runs)
}

// heldRunOptions says what the run is put through while it sleeps.
type heldRunOptions struct {
	residency           bool
	heartbeatRightAfter bool
	// settle, when set, terminates or cancels the run while it sleeps.
	settle func(context.Context, engine.WorkflowStore, string) error
	// storeDownAtWake makes the heartbeat call fail from the moment the run is asleep, so the claim check at the wake cannot get an answer.
	storeDownAtWake bool
	// replacedAtHold registers a successor execution of the run (the next generation) in w.inflight while the run is held, which is what this worker re-claiming a
	// run it still holds does. The store is not touched: its lease is still the held execution's, so nothing but the registration says the execution was replaced.
	replacedAtHold bool
}

func runTerminatedWhileSleeping(t *testing.T, o heldRunOptions) heldRunOutcome {
	t.Helper()
	ctx := context.Background()
	db := testutil.SuiteTestDB(t, "cleat_worker")
	realStore := engine.NewPostgresStore(db)
	flaky := &heartbeatFailingStore{WorkflowStore: realStore}
	var store engine.WorkflowStore = realStore
	if o.storeDownAtWake {
		store = flaky
	}
	residency := o.residency

	wasmBytes := buildResidentSleepFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	defName := fmt.Sprintf("held-terminate-%d", time.Now().UnixNano())
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("held-terminate-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"SleepLongThenCall"}`), "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	calls := &recordedCalls{}
	var mu sync.Mutex
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)
	w.id = "held-terminate-worker"
	w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
	if residency {
		cfg := engine.DefaultResidencyConfig()
		cfg.MaxRuns = 2
		set, err := engine.NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		w.resident = set
	}

	claimed := claimOne(t, ctx, store, w.id, wfID)
	w.inflight.Store(wfID, claimed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runExecuteWorkflow(w, claimed)
	}()

	out := heldRunOutcome{}
	// Both scenarios are settled DURING the sleep, the same way. Residency on, the run is in the resident set. Residency off the run is a suspended row: the
	// worker re-claims its own suspended run within the same executeWorkflow call (a suspend makes this worker claim it again milliseconds later), so the
	// execution does NOT return at the sleep, and settling after it returns would settle a finished run, which this test did on its first attempt.
	deadline := time.Now().Add(5 * time.Second)
	sleeping := func() bool {
		if residency {
			return w.resident.Len() > 0
		}
		st := statusOf(t, ctx, store, wfID)
		return st != "running" && st != "done"
	}
	for !sleeping() && time.Now().Before(deadline) {
		select {
		case <-done:
			deadline = time.Now() // the run ended without ever sleeping
		case <-time.After(2 * time.Millisecond):
		}
	}
	out.heldAtTerminate = sleeping()
	if residency {
		out.statusAtSettle = "held"
	} else {
		out.statusAtSettle = statusOf(t, ctx, store, wfID)
	}

	settledAt := time.Now()
	if o.storeDownAtWake {
		flaky.fail.Store(true)
	}
	if o.settle != nil {
		if err := o.settle(ctx, store, wfID); err != nil {
			t.Fatalf("settling the run (residency=%v, status before: %s): %v\nworker logs:\n%s", residency, out.statusAtSettle, err, logs.String())
		}
	}
	if o.replacedAtHold {
		registerInflightExecution(w, wfID, defName, claimed.Generation+1)
	}
	if o.heartbeatRightAfter {
		// The ticker firing straight after the settle, which is the best case: the fenced heartbeat sees the lease's generation move and fences the run (setup.go).
		w.heartbeatAndFenceInFlight()
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the execution never stopped (residency=%v; logs:\n%s)", residency, logs.String())
	}
	out.timeToEviction = time.Since(settledAt)
	// Give a late call a moment to arrive; the held run's timer fires up to 8 s after the sleep began.
	time.Sleep(500 * time.Millisecond)
	out.reachedTheCall = calls.has("work/after_sleep")
	out.finalStatus = statusOf(t, ctx, store, wfID)
	if out.finalStatus == "ready" || out.finalStatus == "running" || out.finalStatus == "terminating" {
		// A run left claimable would be picked up by whichever test next claims from this shared database (and the package's own guard refuses to leave one),
		// so a scenario that deliberately suspends it settles it once its status has been read.
		if err := realStore.TerminateWorkflow(ctx, wfID, "held-run test cleanup"); err != nil {
			t.Errorf("cleaning up the run that was left %q: %v", out.finalStatus, err)
		}
	}
	out.logs = logs.String()
	out.generationAtEnd = generationOf(t, ctx, db, wfID)
	out.generationAtClaim = claimed.Generation
	return out
}

func generationOf(t *testing.T, ctx context.Context, db *sql.DB, id string) int64 {
	t.Helper()
	var g int64
	if err := db.QueryRowContext(ctx, `SELECT generation FROM workflow_leases WHERE id = $1`, id).Scan(&g); err != nil {
		t.Fatalf("reading the generation of %s: %v", id, err)
	}
	return g
}

func terminateRun(ctx context.Context, s engine.WorkflowStore, id string) error {
	return s.TerminateWorkflow(ctx, id, "held-run terminate test")
}
func cancelRun(ctx context.Context, s engine.WorkflowStore, id string) error {
	return s.CancelWorkflow(ctx, id, "held-run cancel test")
}

// The scenarios differ in WHEN the worker's heartbeat runs after the settle. The heartbeat is how a worker learns a run was terminated, reclaimed or cancelled: it
// sees a lease whose generation moved and fences the run. It runs on an interval (5 s by default), so for that long after a settle a held run does not know.
//
//   - "heartbeat right after" is the best case: the ticker fires at once. It shows the fence path evicts a held run.
//   - "heartbeat withheld" is the worst case inside one interval: the held run's timer fires before the ticker does. It shows whether the guest can act in that window.
func TestATerminatedOrCancelledRunDoesNotActAfterItsSleepWhetherOrNotItIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle func(context.Context, engine.WorkflowStore, string) error
	}{
		{"terminate", terminateRun},
		{"cancel", cancelRun},
	} {
		for _, hb := range []struct {
			name  string
			right bool
		}{
			{"heartbeat right after", true},
			{"heartbeat withheld past the timer", false},
		} {
			t.Run(tc.name+"/"+hb.name, func(t *testing.T) {
				off := runTerminatedWhileSleeping(t, heldRunOptions{residency: false, heartbeatRightAfter: hb.right, settle: tc.settle})
				on := runTerminatedWhileSleeping(t, heldRunOptions{residency: true, heartbeatRightAfter: hb.right, settle: tc.settle})
				t.Logf("%s, %s, residency OFF (control): asleep = %v (status %q), reached the call after the sleep = %v, execution stopped %s after the settle, final status %q",
					tc.name, hb.name, off.heldAtTerminate, off.statusAtSettle, off.reachedTheCall, off.timeToEviction.Round(time.Millisecond), off.finalStatus)
				t.Logf("%s, %s, residency ON:            held = %v, reached the call after the sleep = %v, execution stopped %s after the settle, final status %q",
					tc.name, hb.name, on.heldAtTerminate, on.reachedTheCall, on.timeToEviction.Round(time.Millisecond), on.finalStatus)
				if !off.heldAtTerminate {
					t.Fatalf("PRECONDITION FAILED: with residency OFF the run was not asleep when it was settled, so the control measured nothing")
				}
				if off.reachedTheCall {
					t.Fatalf("PRECONDITION FAILED: with residency OFF the guest reached the call after the sleep, so this scenario does not show what a settled suspended run does")
				}
				if !on.heldAtTerminate {
					t.Fatalf("PRECONDITION FAILED: the run was not held when it was settled, so this measured nothing about a held run")
				}
				if on.reachedTheCall {
					t.Errorf("a run that was %s while HELD still reached its next call, which a suspended run cannot do (%s)", tc.name, hb.name)
				}
			})
		}
	}
}

// A held run whose claim is STILL good must still be held through its sleep and finish in place: the claim check is one round trip at the wake and must not turn a
// hold into a suspension. This is the control for the tests above, which would pass just as well if the check refused every wake.
func TestAHeldRunWhoseClaimIsStillGoodFinishesInPlaceAndIsNotSuspended(t *testing.T) {
	on := runTerminatedWhileSleeping(t, heldRunOptions{residency: true})
	t.Logf("residency ON, nothing done to the run: held = %v, reached the call = %v, execution stopped %s after the hold began, final status %q, generation %d at the claim and %d at the end",
		on.heldAtTerminate, on.reachedTheCall, on.timeToEviction.Round(time.Millisecond), on.finalStatus, on.generationAtClaim, on.generationAtEnd)
	if !on.heldAtTerminate {
		t.Fatalf("PRECONDITION FAILED: the run was not held, so this measured nothing")
	}
	if !on.reachedTheCall || on.finalStatus != "done" {
		t.Errorf("a held run with a good claim did not finish: reached the call = %v, final status %q", on.reachedTheCall, on.finalStatus)
	}
	if strings.Contains(on.logs, "could not revalidate") || strings.Contains(on.logs, "no longer holds its claim") {
		t.Errorf("the claim check refused a wake for a claim that was good:\n%s", on.logs)
	}
	if on.generationAtEnd != on.generationAtClaim {
		t.Errorf("the generation moved from %d to %d: the run was suspended and re-claimed instead of held to its end", on.generationAtClaim, on.generationAtEnd)
	}
}

// A claim that CANNOT be checked at the wake (the store is unreachable) is treated as lost: the wait is suspended instead of resumed. Resumed, the guest would reach the
// call; suspended, it does not, and the run is left claimable ("ready") for whichever worker claims it next, which replays it. This harness runs no dispatch loop, so
// nothing claims it here and the status is what shows the suspension. The worker also says why. An unanswerable question is not a vouching.
func TestAHeldRunWhoseClaimCannotBeCheckedAtItsWakeIsSuspendedNotResumed(t *testing.T) {
	on := runTerminatedWhileSleeping(t, heldRunOptions{residency: true, storeDownAtWake: true})
	t.Logf("residency ON, the heartbeat store unreachable at the wake: held = %v, reached the call = %v, final status before cleanup %q", on.heldAtTerminate, on.reachedTheCall, on.finalStatus)
	if !on.heldAtTerminate {
		t.Fatalf("PRECONDITION FAILED: the run was not held, so this measured nothing")
	}
	if !strings.Contains(on.logs, "could not revalidate its claim") {
		t.Errorf("the worker did not report that the claim could not be revalidated, so the check did not run or did not fail:\n%s", on.logs)
	}
	if on.reachedTheCall {
		t.Errorf("the guest reached the call after a wake whose claim nobody could vouch for: it was resumed in place, not suspended")
	}
	if on.finalStatus != "ready" {
		t.Errorf("final status %q, want \"ready\": a suspended run is left claimable", on.finalStatus)
	}
}

// The end-to-end rows above cannot tell whether the claim check ITSELF reports a lost claim: a run whose check wrongly says "held" is still stopped from making its
// outbound call, by the fence the lost-claim path sets (fencedRuns refuses the next durable call), so the call is not reached either way. What differs is that the guest
// is resumed from its wait and refused at its next call, instead of suspended at the wait. So this reads the check's own answer.
func TestTheClaimCheckAnswersHeldLostAndUnknownAsThreeDifferentThings(t *testing.T) {
	ctx := context.Background()
	db := testutil.SuiteTestDB(t, "cleat_worker")
	realStore := engine.NewPostgresStore(db)
	flaky := &heartbeatFailingStore{WorkflowStore: realStore}

	wasmBytes := buildResidentSleepFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	defName := fmt.Sprintf("claim-check-%d", time.Now().UnixNano())
	if err := realStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, flaky, logs)
	w.id = "claim-check-worker"

	registered := map[string]context.Context{}
	claimRun := func(suffix string) *engine.WorkflowInstance {
		id := fmt.Sprintf("claim-check-%s-%d", suffix, time.Now().UnixNano())
		if _, _, err := realStore.StartNewRun(ctx, id, defName, meta.WorkflowVersion, json.RawMessage(`{"__entry_point":"SleepLongThenCall"}`), "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		wf := claimOne(t, ctx, realStore, w.id, id)
		w.inflight.Store(id, wf)
		// The run is EXECUTING in the real flow, and fenceOutLostRun sets nothing for a run with no registered execution ("already gone, nothing to cancel"), so this one is
		// registered the way executeWorkflow registers it.
		execCtx, cancel := context.WithCancel(ctx)
		w.execCancel.Store(id, &execRegistration{generation: wf.Generation, cancel: cancel})
		t.Cleanup(func() { cancel(); _ = realStore.TerminateWorkflow(ctx, id, "claim check test cleanup") })
		registered[id] = execCtx
		return wf
	}
	fenced := func(wf *engine.WorkflowInstance) bool {
		v, ok := w.fencedRuns.Load(wf.ID)
		return ok && v.(int64) == wf.Generation
	}

	// HELD: assigned to this worker at its generation.
	held := claimRun("held")
	if err := w.claimStillMine(ctx, held); err != nil {
		t.Errorf("a run this worker holds at its generation was reported %v, want nil", err)
	}
	if fenced(held) {
		t.Errorf("a held run was marked fenced")
	}

	// LOST: terminated in the store, which bumps the generation and clears the assignment.
	lost := claimRun("lost")
	if err := realStore.TerminateWorkflow(ctx, lost.ID, "claim check test"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
	if err := w.claimStillMine(ctx, lost); !errors.Is(err, errClaimLost) {
		t.Errorf("a terminated run was reported %v, want errClaimLost", err)
	}
	if !fenced(lost) {
		t.Errorf("a run reported lost was not marked fenced, so its next durable call would not be refused")
	}
	if registered[lost.ID].Err() == nil {
		t.Errorf("a run reported lost still has a live execution context: the lost-claim path did not cancel it")
	}

	// UNKNOWN: the store cannot be asked. Not lost (nothing says it was), so it is NOT marked fenced and its execution is not cancelled; the wait suspends and the run is
	// replayed. The heartbeat makes the same choice for the same reason: one blip must not stop every execution.
	unknown := claimRun("unknown")
	flaky.fail.Store(true)
	err = w.claimStillMine(ctx, unknown)
	flaky.fail.Store(false)
	if err == nil || errors.Is(err, errClaimLost) {
		t.Errorf("an unanswerable check returned %v, want a non-nil error that is NOT errClaimLost", err)
	}
	if fenced(unknown) {
		t.Errorf("an unanswerable check marked the run fenced: one failed read would stop an execution that was fine")
	}
	if registered[unknown.ID].Err() != nil {
		t.Errorf("an unanswerable check cancelled the execution")
	}
}

// cleat-review3 on #4276: a run this worker re-claims while it still holds the previous execution replaces that execution in w.inflight, so the tick never covers the
// held one and a check that read only the fence marker said "still mine". The unit tests pin the answer; this one runs it through a real held run.
//
// WHAT IT CAN AND CANNOT SEE. After the held execution suspends, this worker re-claims the run and replays it, and the replay makes the call too, so the service's
// call count cannot tell "the held execution acted" from "its replacement did". The check's own WARN line can: it is written only on the lost path, so it is the
// observable that says the held execution was sent back at its wake and not resumed.
func TestAHeldExecutionThatThisWorkerReplacedWhileItWasHeldIsSentBackAtItsWake(t *testing.T) {
	const lostLine = "no longer holds its claim at its wake"
	replaced := runTerminatedWhileSleeping(t, heldRunOptions{residency: true, replacedAtHold: true})
	control := runTerminatedWhileSleeping(t, heldRunOptions{residency: true})

	if !replaced.heldAtTerminate || !control.heldAtTerminate {
		t.Fatalf("PRECONDITION FAILED: the run was not held (replaced=%v, control=%v), so nothing about a held run was measured", replaced.heldAtTerminate, control.heldAtTerminate)
	}
	if strings.Contains(control.logs, lostLine) {
		t.Fatalf("PRECONDITION FAILED: the control, which replaced nothing, logged %q, so the line does not mean what this test reads it as:\n%s", lostLine, control.logs)
	}
	if !strings.Contains(replaced.logs, lostLine) {
		t.Errorf("a held execution this worker had replaced was resumed at its wake (no %q in the worker's log):\n%s", lostLine, replaced.logs)
	}
}
