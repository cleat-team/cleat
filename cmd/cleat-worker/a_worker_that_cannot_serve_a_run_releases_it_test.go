package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1710: a worker that cannot serve a run used to CLAIM it and then
// destroy it.
//
// Both pre-flight checks failed on facts local to the worker -- its loaded
// plugins, and the WASM binary it had loaded -- and both answered
// recordTerminalFailure with engine.ErrPermanent. So a pool whose workers did
// not all satisfy the same plugin versions did not route work, it killed a
// claim-race-determined fraction of it, and a rolling plugin deploy was
// precisely the heterogeneous state that triggered it.
//
// The predicate these tests pin: A PRE-FLIGHT CHECK THAT FAILS ON A
// WORKER-LOCAL FACT MUST RELEASE, NOT TERMINATE. The boundary matters as much
// as the rule -- a run-intrinsic failure is wrong on every worker, and
// releasing it would circulate a run that can never run.

// releaseRecord is one observed ReleaseWorkflow call.
type releaseRecord struct {
	workflowID string
	workerID   string
	generation int64
	nextWakeAt time.Time
}

// unservableFixture is a worker whose store records releases and FAILS LOUDLY
// on any terminal write, so "it did not terminate" is asserted rather than
// inferred from the absence of a record.
func unservableFixture(t *testing.T, releaseErr error) (*Worker, *[]releaseRecord) {
	t.Helper()
	var mu sync.Mutex
	var releases []releaseRecord

	ms := &mockStore{
		releaseWorkflowFn: func(_ context.Context, id, workerID string, gen int64, next time.Time) error {
			mu.Lock()
			releases = append(releases, releaseRecord{id, workerID, gen, next})
			mu.Unlock()
			return releaseErr
		},
		failWorkflowFn: func(_ context.Context, id, _ string, _ int64, errMsg, errorCode, errorOp string, _ map[string]string) error {
			t.Errorf("FailWorkflow was called for %s (code=%q op=%q msg=%q).\n\n"+
				"This is cleat#1710: a pre-flight check that fails on a worker-local "+
				"fact must return the run to the queue for a worker that can serve it, "+
				"not destroy work a sibling could have done.", id, errorCode, errorOp, errMsg)
			return nil
		},
		moveToDeadLetterQueueFn: func(_ context.Context, id, _ string, _ int64, _, _, _ string) error {
			t.Errorf("MoveToDeadLetterQueue was called for %s -- see cleat#1710", id)
			return nil
		},
	}
	w := newTestWorker(ms)
	w.unservableBackoff = 30 * time.Second
	return w, &releases
}

func unservableRun() *engine.WorkflowInstance {
	return &engine.WorkflowInstance{
		ID: "wf-1710", DefName: "billing", DefVersion: 7, Generation: 3,
		Status: "running", AssignedTo: "test-worker",
	}
}

// THE HEADLINE. The run goes back to the queue rather than being failed.
func TestAWorkerThatCannotServeARunReleasesItInsteadOfDestroyingIt(t *testing.T) {
	w, releases := unservableFixture(t, nil)
	wf := unservableRun()

	before := time.Now()
	w.releaseForAnotherWorker(wf, `plugin "llm" version 1.0.0 does not satisfy ">=2.0.0"`, "plugin_check")

	if len(*releases) != 1 {
		t.Fatalf("ReleaseWorkflow was called %d times, want 1.\n\n"+
			"Before cleat#1710 this path called recordTerminalFailure with "+
			"engine.ErrPermanent, so a worker that could not serve a run claimed it "+
			"and killed it.", len(*releases))
	}
	got := (*releases)[0]
	if got.workflowID != wf.ID || got.workerID != w.id || got.generation != wf.Generation {
		t.Errorf("released (%s, %s, gen %d), want (%s, %s, gen %d) -- the release must "+
			"be fenced on the same claim it is giving up",
			got.workflowID, got.workerID, got.generation, wf.ID, w.id, wf.Generation)
	}
	if !got.nextWakeAt.After(before) {
		t.Errorf("next_wake_at is %v, which is not in the future.\n\n"+
			"A release with no backoff lets the releasing worker win the same run "+
			"straight back and spin on it.", got.nextWakeAt)
	}
}

// The backoff is the pool-wide bound on a run nothing can serve, so it has to
// be the CONFIGURED one rather than an arbitrary future instant.
func TestTheReleaseBackoffIsTheConfiguredOne(t *testing.T) {
	w, releases := unservableFixture(t, nil)
	w.unservableBackoff = 90 * time.Second

	before := time.Now()
	w.releaseForAnotherWorker(unservableRun(), "mismatch", "version_check")
	after := time.Now()

	if len(*releases) != 1 {
		t.Fatalf("want exactly one release, got %d", len(*releases))
	}
	wake := (*releases)[0].nextWakeAt
	if wake.Before(before.Add(90*time.Second)) || wake.After(after.Add(90*time.Second)) {
		t.Errorf("next_wake_at is %v, want ~%v (now + the configured 90s).\n\n"+
			"ReleaseWorkflow writes next_wake_at on the ROW, so this value is what "+
			"throttles the WHOLE POOL on a run nobody can serve. If it is ignored, "+
			"that run is claimed and released as fast as the dispatch loop turns.",
			wake, before.Add(90*time.Second))
	}
}

// A zero backoff must not mean "no backoff".
func TestAZeroBackoffFallsBackToTheDefaultRatherThanSpinning(t *testing.T) {
	w, releases := unservableFixture(t, nil)
	w.unservableBackoff = 0

	before := time.Now()
	w.releaseForAnotherWorker(unservableRun(), "mismatch", "plugin_check")

	if len(*releases) != 1 {
		t.Fatalf("want exactly one release, got %d", len(*releases))
	}
	if wake := (*releases)[0].nextWakeAt; !wake.After(before.Add(defaultUnservableBackoff / 2)) {
		t.Errorf("a zero backoff produced next_wake_at %v, which is ~now.\n\n"+
			"Zero must mean defaultUnservableBackoff (%v), not 'claimable "+
			"immediately' -- a worker would win the same run back and spin.",
			wake, defaultUnservableBackoff)
	}
}

// A lost fence is the outcome this function wants, not an error -- and it still
// must not terminate.
func TestALostFenceOnReleaseIsNotATerminalFailure(t *testing.T) {
	w, releases := unservableFixture(t, engine.ErrFenceLost)
	w.releaseForAnotherWorker(unservableRun(), "mismatch", "plugin_check")
	if len(*releases) != 1 {
		t.Fatalf("want the release to have been attempted once, got %d", len(*releases))
	}
	// The fixture fails the test if any terminal write happened.
}

// A release that genuinely fails must leave the run claimed until its lease
// expires -- the reaper then returns it. It must NOT fall back to terminating.
func TestAFailedReleaseDoesNotFallBackToDestroyingTheRun(t *testing.T) {
	w, releases := unservableFixture(t, errors.New("connection refused"))
	w.releaseForAnotherWorker(unservableRun(), "mismatch", "version_check")
	if len(*releases) != 1 {
		t.Fatalf("want the release to have been attempted once, got %d", len(*releases))
	}
	// The fixture fails the test if any terminal write happened. Slower than a
	// release -- the lease has to expire -- but the same destination, and the
	// run survives.
}

// The two pre-flight checks stay on the releasing path.
//
// A UNIT TEST OF THE HELPER CANNOT SEE THIS. executeWorkflow needs a real WASM
// binary to reach either check, so the thing a regression would actually do --
// point one of them back at recordTerminalFailure -- is invisible to every test
// above. This reads the source instead and keys on the op strings, which is
// precisely what such a regression has to carry.
func TestNeitherPreflightCheckTerminatesTheRun(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "setup.go", nil, 0)
	if err != nil {
		t.Fatalf("parse setup.go: %v", err)
	}

	// Where each op string is used, and by which function.
	usedBy := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			if s == "version_check" || s == "plugin_check" {
				usedBy[s] = append(usedBy[s], sel.Sel.Name)
			}
		}
		return true
	})

	for _, op := range []string{"version_check", "plugin_check"} {
		callers := usedBy[op]
		if len(callers) == 0 {
			t.Errorf("no call in setup.go passes %q.\n\n"+
				"This guard keys on that string. If the check was renamed, rename it "+
				"here too -- do not delete the case, or cleat#1710 becomes "+
				"unguarded silently.", op)
			continue
		}
		for _, fn := range callers {
			if fn != "releaseForAnotherWorker" {
				t.Errorf("%q is passed to %s, want releaseForAnotherWorker.\n\n"+
					"cleat#1710: both pre-flight checks fail on WORKER-LOCAL facts -- "+
					"this worker's loaded plugins, this worker's WASM binary. Another "+
					"worker may serve the run, so terminating it destroys work a "+
					"sibling could have done, non-deterministically, and a rolling "+
					"deploy is exactly the state that triggers it.", op, fn)
			}
		}
	}
}
