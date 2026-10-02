package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2942, the worker half. A run that was STOPPED -- the worker is shutting
// down, or the run was fenced out to a later generation -- must be released,
// never failed. The fence half is the one that was missing: the fence cancels
// this execution's context (cleat#2008 fix 1), that cancellation lands on
// ordinary context-taking reads (the event verifier, the history load, the
// tenant pool), and each of them turned it into a terminal failure a caller
// cannot tell from a real one.
//
// Releases are RECORDED and any terminal write FAILS LOUDLY, so "it did not
// fail the run" is asserted rather than inferred from the absence of a record.
// That is the pattern a_worker_that_cannot_serve_a_run_releases_it_test.go
// established, and it is what stops this being satisfied by a guard that
// silently does nothing.

type stoppedRunFixture struct {
	w        *Worker
	releases int
}

func newStoppedRunFixture(t *testing.T) *stoppedRunFixture {
	t.Helper()
	f := &stoppedRunFixture{}
	ms := &mockStore{
		// Enough for executeWorkflow to get as far as eng.Replay: a def whose
		// module loads. Nothing here runs a guest -- Replay fails on its own,
		// which is the point (see the post-Replay test below).
		loadWASMFn: func(_ context.Context, _ string, _ int) ([]byte, error) {
			return []byte{
				0x00, 0x61, 0x73, 0x6d, // magic
				0x01, 0x00, 0x00, 0x00, // version 1
			}, nil
		},
		releaseWorkflowFn: func(_ context.Context, _ string, _ string, _ int64, _ time.Time) error {
			f.releases++
			return nil
		},
		failWorkflowFn: func(_ context.Context, id, _ string, _ int64, errMsg, errorCode, errorOp string, _ map[string]string) error {
			t.Errorf("FailWorkflow was called for %s (code=%q op=%q msg=%q).\n\n"+
				"A stopped run must be released, not failed: cleat#2942. The caller cannot tell a "+
				"fence cancel from an application failure, and fencing exists to hand a run over.",
				id, errorCode, errorOp, errMsg)
			return nil
		},
	}
	f.w = newTestWorker(ms)
	return f
}

func (f *stoppedRunFixture) wf() *engine.WorkflowInstance {
	return &engine.WorkflowInstance{ID: "run-1", DefName: "test-def", TenantID: "t-1", Generation: 3}
}

func TestAFencedRunIsReleasedRatherThanFailed(t *testing.T) {
	f := newStoppedRunFixture(t)
	wf := f.wf()
	// What the heartbeat fence leaves behind for the generation it judged.
	f.w.fencedRuns.Store(wf.ID, wf.Generation)

	if !f.w.releaseIfStopped(wf, "history load", context.Canceled) {
		t.Fatal("releaseIfStopped = false for a fenced run -- the guard would fall through to the failure path")
	}
	if f.releases != 1 {
		t.Errorf("releases = %d, want 1 -- a fenced run must go back to the queue", f.releases)
	}
}

func TestAShuttingDownWorkerReleasesRatherThanFails(t *testing.T) {
	f := newStoppedRunFixture(t)
	wf := f.wf()
	f.w.cancel() // w.ctx.Err() != nil

	if !f.w.releaseIfStopped(wf, "history load", context.Canceled) {
		t.Fatal("releaseIfStopped = false while shutting down -- cleat#2285's rule must still hold")
	}
	if f.releases != 1 {
		t.Errorf("releases = %d, want 1", f.releases)
	}
}

// The control, without which the two above are satisfied by a guard that
// releases everything -- including runs that genuinely failed on a live worker.
func TestALiveWorkerWithAnUnfencedRunDoesNotRelease(t *testing.T) {
	f := newStoppedRunFixture(t)
	wf := f.wf()

	if f.w.releaseIfStopped(wf, "history load", context.Canceled) {
		t.Fatal("releaseIfStopped = true for a live worker and an unfenced run -- a real failure would be swallowed")
	}
	if f.releases != 0 {
		t.Errorf("releases = %d, want 0 -- nothing was stopped, so nothing may be handed over", f.releases)
	}
}

// The predicate's two clauses, plainly. NOT asserted here: that the marker is
// per-generation. It is not -- runIsFenced loads by id and ignores the stored
// value -- so a test claiming otherwise would be asserting a property the code
// does not have. See the note on the PR: a predecessor's marker lingering past
// a successor's claim would refuse the successor's durable calls, which is
// worth its own look rather than being smuggled in here.
func TestRunWasStoppedPredicate(t *testing.T) {
	f := newStoppedRunFixture(t)
	wf := f.wf()

	if f.w.runWasStopped(wf) {
		t.Error("runWasStopped = true for a live worker and an unfenced run")
	}
	// The value is the generation the fence judged (PR #2950). runIsFenced
	// loads by id and ignores it -- see cleat#2956 -- so this is here to keep
	// the fixture faithful to what the fence actually writes, not because the
	// predicate reads it.
	f.w.fencedRuns.Store(wf.ID, wf.Generation)
	if !f.w.runWasStopped(wf) {
		t.Error("runWasStopped = false for a fenced run")
	}

	g := newStoppedRunFixture(t)
	g.w.cancel()
	if !g.w.runWasStopped(g.wf()) {
		t.Error("runWasStopped = false while shutting down")
	}
}

// The post-Replay call site (setup.go:3761), DRIVEN rather than assumed.
//
// The three tests above call releaseIfStopped directly, which pins the helper
// and says nothing about whether executeWorkflow still calls it: deleting that
// one line left the whole cmd/cleat-worker package green (measured 2026-10-02,
// by cleat-review and reproduced here). Between the fence and the release there
// are 600-odd lines of real work, so this runs the real sequence instead.
//
// Replay is allowed to fail here, and that is the case this guard exists for:
// the fence cancels execCtx, the cancellation lands on a read the engine makes
// before the guest runs, and what comes back is an ERROR that must be read as
// "this segment was cut off", not "this workflow failed".
func TestAFencedRunIsReleasedAtThePostReplayBranch(t *testing.T) {
	f := newStoppedRunFixture(t)
	wf := f.wf()
	wf.DefVersion = 1
	// Naming the entry point in the input is what lets a bare module through
	// determineEntryPoint without a cleat.metadata section.
	wf.Input = json.RawMessage(`{"__entry_point":"test"}`)
	// What the heartbeat fence leaves behind for the generation it judged.
	f.w.fencedRuns.Store(wf.ID, wf.Generation)

	// Capture the log, because the release NAME is what says WHICH site did it.
	// `releases == 1` alone would also be satisfied by a release from the
	// history-load or tenant-pool guard, so a test asserting only that would
	// still pass if this branch were dropped. releaseIfStopped logs the site it
	// fired at, so assert on that text.
	var logs bytes.Buffer
	f.w.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	f.w.wg.Add(1) // executeWorkflow defers w.wg.Done()
	f.w.executeWorkflow(wf)

	if f.releases != 1 {
		t.Errorf("releases = %d, want 1 -- a fenced run must be handed back at the post-Replay branch "+
			"(setup.go:3761); if this is 0 the call there was dropped and the run is being failed instead",
			f.releases)
	}
	if !strings.Contains(logs.String(), "at=post-replay") {
		t.Errorf("the release did not come from the post-Replay branch.\n\n"+
			"Want a release logged at=post-replay (setup.go:3761). Got:\n%s\n"+
			"If this branch is unreachable in the test, a release from an earlier guard is being "+
			"mistaken for it and this test does not pin the site it names.", logs.String())
	}
}
