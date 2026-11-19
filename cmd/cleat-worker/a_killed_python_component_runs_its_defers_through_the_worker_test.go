package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4079, through a worker: a Python component KILLED by the execution fence with a defer registered runs that defer, on a
// fresh instance, through the two-phase transition, and the run ends failed with the failure it was killed with.
//
// A trapped component instance cannot be re-entered, so before this the cleanup of a killed component run was lost. The route is:
// the kill marks the run 'terminating' with its outcome and error (markKilledRunForItsDefers), the next claim replays its history on
// a new instance and stops at the last stoppable event (engine.WithStopAtLastStoppable), the guest's defers run, and
// FinalizeDeferPhase applies the recorded outcome.
//
// EVERYTHING HERE IS REAL: the Python components (cleat build --target python), wasmtime and its epoch fence, the engine, the worker's
// executeWorkflow, the store and PostgreSQL. The service the calls reach is an httptest server that records what it was asked.

func TestAKilledPythonComponentRunsItsDefersThroughTheWorker(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed killed-run test")
	}
	ctx := context.Background()

	calls := &recordedCalls{}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		calls.add(op)
		w.Header().Set("Content-Type", "application/json")
		if op == "work/flaky" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	// The ordinary-retry control retries a dead-lettered run, which is then `ready` by design, and a ready run left behind is claimable
	// by the next test that reads "the next ready run" (cleat#3614).
	settleRunsLeftBehind(t, db)
	store := engine.NewPostgresStore(db)
	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)
	w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
	// The fence that kills a spinning guest. Short, so the test does not wait on it, and long enough that an ordinary segment
	// is never near it.
	w.wasmInstanceTimeout = 2 * time.Second

	count := func(op string) int {
		n := 0
		for _, got := range calls.all() {
			if got == op {
				n++
			}
		}
		return n
	}

	// run builds one entry, starts a run of it and returns the run id and a function that executes its next claim.
	run := func(t *testing.T, entry string) (string, func()) {
		t.Helper()
		wasmBytes := buildPythonExample(t, filepath.Join("python-sdk", "examples", "killed_with_defers_workflow.py"), entry)
		meta, err := wasm.ReadMetadata(wasmBytes)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		defName := fmt.Sprintf("killed-%s-%d", strings.ReplaceAll(entry, "_", "-"), time.Now().UnixNano())
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
			ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
		}); err != nil {
			t.Fatalf("deploy %s: %v", entry, err)
		}
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("service saw %v\nworker logs:\n%s", calls.all(), logs.String())
			}
		})
		id := fmt.Sprintf("%s-run", defName)
		if _, _, err := store.StartNewRun(ctx, id, defName, meta.WorkflowVersion, json.RawMessage(`{"request":{}}`), "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		return id, func() {
			// This worker runs no heartbeat loop, so left alone it reads as "fence lost" once reclaimAfter() has passed and
			// refuses every new durable call (heartbeatPresumedLost): a defer's call included. The first shape of a long test
			// is fine and the later ones are not, which looks like a defect in the code under test. A real worker beats.
			w.lastHeartbeatOK.Store(time.Now().UnixNano())
			runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, id))
		}
	}
	get := func(t *testing.T, id string) *engine.WorkflowInstance {
		t.Helper()
		wf, err := store.GetWorkflowByID(ctx, id)
		if err != nil || wf == nil {
			t.Fatalf("GetWorkflowByID(%s): %v", id, err)
		}
		return wf
	}
	history := func(t *testing.T, id string) []engine.EventRecord {
		t.Helper()
		h, err := store.LoadEventHistory(ctx, id)
		if err != nil {
			t.Fatalf("LoadEventHistory (verifies the checksum chain): %v", err)
		}
		return h
	}
	segmentEvents := func(h []engine.EventRecord) (marked int) {
		for _, ev := range h {
			if ev.InDeferSegment {
				marked++
			}
		}
		return marked
	}

	t.Run("a call then a loop: the kill is routed through a segment and the cleanup runs", func(t *testing.T) {
		id, step := run(t, "spin_after_call")
		start := time.Now()
		step() // the kill
		if got := get(t, id).Status; got != "terminating" {
			t.Fatalf("status after the kill = %q, want terminating: the run owes a defer and must be marked for its segment (logs:\n%s)", got, logs.String())
		}
		killed := get(t, id)
		if !strings.Contains(killed.Error, "time limit") {
			t.Fatalf("the recorded error is %q; the run must already report what killed it", killed.Error)
		}
		if n := count("cleanup/refund"); n != 0 {
			t.Fatalf("PRECONDITION FAILED: the cleanup ran %d time(s) before the segment", n)
		}
		if n := count("work/charge"); n != 1 {
			t.Fatalf("PRECONDITION FAILED: work/charge ran %d time(s) in the killed run, want 1", n)
		}

		step() // the defer segment
		done := get(t, id)
		if done.Status != "failed" {
			t.Fatalf("status after the segment = %q, want failed (logs:\n%s)", done.Status, logs.String())
		}
		if !strings.Contains(done.Error, "time limit") || done.ErrorCode != killed.ErrorCode {
			t.Fatalf("the failure changed: error %q code %q, was error %q code %q", done.Error, done.ErrorCode, killed.Error, killed.ErrorCode)
		}
		if n := count("cleanup/refund"); n != 1 {
			t.Fatalf("the cleanup ran %d time(s), want exactly 1 (service saw %v)", n, calls.all())
		}
		if n := count("work/charge"); n != 1 {
			t.Fatalf("work/charge ran %d time(s) in total, want 1: the segment must replay the recorded call, never repeat it", n)
		}
		h := history(t, id)
		if segmentEvents(h) == 0 {
			t.Fatalf("no event in the history is marked as the segment's: %+v", h)
		}
		if elapsed := time.Since(start); elapsed > 60*time.Second {
			t.Fatalf("took %v: the segment did not stop at the last stoppable event, it ran until killed again", elapsed)
		}
		if !strings.Contains(logs.String(), "marked for a defer segment") {
			t.Fatal("the worker did not log routing the kill through a segment")
		}
	})

	t.Run("a defer then a loop: the stop is the registration itself", func(t *testing.T) {
		id, step := run(t, "spin_after_defer")
		step()
		if got := get(t, id).Status; got != "terminating" {
			t.Fatalf("status after the kill = %q, want terminating (logs:\n%s)", got, logs.String())
		}
		step()
		if got := get(t, id).Status; got != "failed" {
			t.Fatalf("status after the segment = %q, want failed (logs:\n%s)", got, logs.String())
		}
		if n := count("cleanup/first"); n != 1 {
			t.Fatalf("the cleanup ran %d time(s), want exactly 1 (service saw %v)", n, calls.all())
		}
	})

	// CONTROLS. Each is a shape the route must NOT touch, and each would pass if the route fired on everything.
	t.Run("CONTROL: a killed run that owes no defers fails at once", func(t *testing.T) {
		id, step := run(t, "spin_no_defer")
		step()
		if got := get(t, id).Status; got != "failed" {
			t.Fatalf("status = %q, want failed straight away: a run with nothing to clean up has no segment (logs:\n%s)", got, logs.String())
		}
	})

	t.Run("CONTROL: an ordinary exception is not a kill and its defer runs once", func(t *testing.T) {
		before := count("cleanup/refund")
		id, step := run(t, "raise_with_defer")
		step()
		wf := get(t, id)
		if wf.Status != "failed" {
			t.Fatalf("status = %q, want failed straight away: the guest's own wrapper ran its defers (logs:\n%s)", wf.Status, logs.String())
		}
		if n := count("cleanup/refund") - before; n != 1 {
			t.Fatalf("the cleanup ran %d time(s), want exactly 1: a second run of it would be a double refund", n)
		}
		if segmentEvents(history(t, id)) != 0 {
			t.Fatal("an ordinary failure's own drain was marked as a segment's")
		}
	})

	// The owner's decision on cleat#4079: a killed run that the classifier calls dead_lettered STILL gets its segment and ends
	// dead_lettered, and RetryWorkflow refuses it because its history now holds a segment's events.
	t.Run("a dead-lettered kill: the segment runs, the run ends dead_lettered, and retry is refused", func(t *testing.T) {
		before := count("cleanup/refund")
		id, step := run(t, "exhaust_then_spin")
		step() // the kill, after the retry policy was exhausted
		killed := get(t, id)
		if killed.Status != "terminating" {
			t.Fatalf("status after the kill = %q, want terminating (logs:\n%s)", killed.Status, logs.String())
		}
		if n := count("cleanup/refund") - before; n != 0 {
			t.Fatalf("PRECONDITION FAILED: the cleanup ran %d time(s) before the segment", n)
		}
		step() // the segment
		done := get(t, id)
		if done.Status != "dead_lettered" {
			t.Fatalf("status after the segment = %q, want dead_lettered: the exhausted call must still classify the run so it keeps its retention (logs:\n%s)", done.Status, logs.String())
		}
		if done.ErrorCode != "retries_exhausted" {
			t.Fatalf("error code %q, want retries_exhausted", done.ErrorCode)
		}
		if n := count("cleanup/refund") - before; n != 1 {
			t.Fatalf("the cleanup ran %d time(s), want exactly 1", n)
		}
		if segmentEvents(history(t, id)) == 0 {
			t.Fatal("no event is marked as the segment's")
		}
		if err := engine.RetryWorkflow(ctx, store, id); !errors.Is(err, engine.ErrAdminStateConflict) {
			t.Fatalf("RetryWorkflow of a dead-lettered run that ran a segment: want ErrAdminStateConflict, got %v", err)
		}
		if got := get(t, id).Status; got != "dead_lettered" {
			t.Fatalf("a refused retry changed the status to %q", got)
		}
	})

	t.Run("CONTROL: an ordinary dead-lettered run, an exhausted call and no kill, still retries", func(t *testing.T) {
		before := count("cleanup/refund")
		id, step := run(t, "exhaust_then_raise")
		step()
		wf := get(t, id)
		if wf.Status != "dead_lettered" {
			t.Fatalf("status = %q, want dead_lettered at once: this is an ordinary failure (logs:\n%s)", wf.Status, logs.String())
		}
		if n := count("cleanup/refund") - before; n != 1 {
			t.Fatalf("the cleanup ran %d time(s), want exactly 1", n)
		}
		if segmentEvents(history(t, id)) != 0 {
			t.Fatal("an ordinary failure's own drain was marked as a segment's")
		}
		if err := engine.RetryWorkflow(ctx, store, id); err != nil {
			t.Fatalf("an ordinary dead-lettered run with its own drain must still retry, got %v", err)
		}
		if got := get(t, id).Status; got != "ready" {
			t.Fatalf("status %q after the retry, want ready", got)
		}
	})

	// cleat#4079: resolve_promise, reject_promise and reply_to_signal (an SDK method over resolve_promise) are effects another run may
	// be waiting on, so a defer segment's body must not make them, and a kill right after one must stop AT it. What tells a stop at
	// the settlement from a stop BEFORE it is what the guest knew when the cleanup ran: the call that precedes the settlement is
	// replayed (so the guest saw it and wrote "charged") only if the replay got as far as the settlement.
	settledEvents := func(h []engine.EventRecord, op string) (original, inSegment int) {
		for _, ev := range h {
			if (ev.EventType != "promise_resolved" && ev.EventType != "promise_rejected") || ev.Op != op || ev.Service != engine.PromiseEventService {
				continue
			}
			if ev.InDeferSegment {
				inSegment++
			} else {
				original++
			}
		}
		return original, inSegment
	}
	for _, shape := range []struct{ entry, op string }{
		{"resolve_then_spin", engine.PromiseOpResolve},
		{"reject_then_spin", engine.PromiseOpReject},
		{"reply_then_spin", engine.PromiseOpResolve},
	} {
		shape := shape
		t.Run("a kill right after "+shape.entry+": the settlement is a stop point, and the cleanup saw the call before it", func(t *testing.T) {
			sawCharged, charged := count("cleanup/saw-charged"), count("work/charge")
			id, step := run(t, shape.entry)
			step() // the kill
			if st := get(t, id).Status; st != "terminating" {
				t.Fatalf("status after the kill = %q, want terminating: a run that owes a defer is marked for its segment", st)
			}
			start := time.Now()
			step() // the segment
			if st := get(t, id).Status; st != "failed" {
				t.Fatalf("status after the segment = %q, want failed (logs:\n%s)", st, logs.String())
			}
			if elapsed := time.Since(start); elapsed > 20*time.Second {
				t.Fatalf("the segment took %v: it was killed again (the guest ran past the settlement), and the 30 s drain budget ended it", elapsed)
			}
			if n := count("cleanup/saw-charged") - sawCharged; n != 1 {
				t.Fatalf("the cleanup saw the charge %d time(s), want exactly 1: the replay must reach the settlement so the guest sees the "+
					"call before it (service saw %v)", n, calls.all())
			}
			if n := count("work/charge") - charged; n != 1 {
				t.Fatalf("the charge ran %d time(s), want exactly 1: a replay consumes a recorded call, it does not repeat it", n)
			}
			original, inSegment := settledEvents(history(t, id), shape.op)
			if original != 1 {
				t.Fatalf("the history holds %d original %s event(s), want exactly 1", original, shape.op)
			}
			if inSegment != 0 {
				t.Fatalf("the defer segment settled the promise itself (%d %s event(s) marked as the segment's): the host must refuse it", inSegment, shape.op)
			}
		})
	}

	t.Run("CONTROL: an await of an already-settled promise is a read, so it is NOT the stop point", func(t *testing.T) {
		sawSettled, charged := count("cleanup/saw-settled"), count("work/charge")
		id, step := run(t, "charge_then_await_settled")
		step() // the kill
		if st := get(t, id).Status; st != "terminating" {
			t.Fatalf("status after the kill = %q, want terminating", st)
		}
		start := time.Now()
		step() // the segment
		if st := get(t, id).Status; st != "failed" {
			t.Fatalf("status after the segment = %q, want failed (logs:\n%s)", st, logs.String())
		}
		// The stop is at the CHARGE call (the last stoppable event), so the guest knows only that the promise was settled. Were the
		// await's promise_resolved event taken for a stop point, the guest would replay through the charge, reach the await, find
		// the promise already settled, get a normal answer back and run into the loop again.
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Fatalf("the segment took %v: it was killed again, which is what a stop placed at the await's event does", elapsed)
		}
		if n := count("cleanup/saw-settled") - sawSettled; n != 1 {
			t.Fatalf("the cleanup saw exactly the settlement %d time(s), want 1 (service saw %v)", n, calls.all())
		}
		if n := count("work/charge") - charged; n != 1 {
			t.Fatalf("the charge ran %d time(s), want exactly 1", n)
		}
	})

	t.Run("a segment that is itself killed still ends the run, promptly", func(t *testing.T) {
		id, step := run(t, "spin_in_defer")
		step() // the kill of the body
		if got := get(t, id).Status; got != "terminating" {
			t.Fatalf("status after the kill = %q, want terminating (logs:\n%s)", got, logs.String())
		}
		start := time.Now()
		step() // the segment, whose defer body loops forever and is killed in turn
		if got := get(t, id).Status; got != "failed" {
			t.Fatalf("status after a killed segment = %q, want failed: finishDeferPhase must apply the recorded outcome whatever the segment did (logs:\n%s)", got, logs.String())
		}
		// Measured 30.2 s: the drain has its own 30 s budget, so a defer body that never returns is cut off there, not by the
		// 2 s fence on the body. The bound is the thing the test is about: the 5-minute defer-phase deadline (deferPhaseTimeout)
		// must not be what ends it. Two minutes leaves the drain budget room and still fails if the run waited for the deadline.
		if elapsed := time.Since(start); elapsed > 2*time.Minute {
			t.Fatalf("a killed segment took %v to end the run; it must not wait for the 5-minute defer-phase deadline", elapsed)
		}
	})
}
