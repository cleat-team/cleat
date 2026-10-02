package main

import (
	"context"
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
