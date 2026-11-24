//go:build cgo

package csharp

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4129: python's await_signals_ms, await_promise_ms and await_child call dispatch_updates() before they wait, so an update that is
// pending when the guest reaches the wait is served rather than left until the wait ends (which may be never). Groups B and C left the
// call out because update handlers were group E and no group PR may depend on another; this pins it now that all three are on develop.
// TestCSharpGroupESleepIsADispatchPoint is the model, for sleep.
//
// One test per wait point, as separate top-level functions: a deleted call must fail ITS test only.

// atWait queues an update, runs the guest into the named wait with nothing to satisfy it, and checks the two things that say the wait is a
// dispatch point: the update was served, and the run still suspended at the wait (so the guest did not run on).
func atWait(t *testing.T, wait string, opts ...engine.EngineOption) {
	t.Helper()
	store := newFakeUpdateStore()
	req, prom := store.add("echo", "via-"+wait)
	res := runConformanceEntry(t, "dispatch_at_wait", entryRun{
		WorkflowID: "wf-dispatch-" + wait,
		Input:      `{"wait":"` + wait + `"}`,
		Options:    append(withUpdates(store), opts...),
	})
	if res.Err != nil {
		t.Fatalf("the run failed: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("the run completed (%s); nothing could satisfy the %s wait, so it should have suspended there", res.Result, wait)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the %s wait and made %v", wait, ops)
	}
	result, errMsg, done := store.outcome(req)
	if !done {
		t.Fatalf("the update pending at the %s wait was not served: it is not a dispatch point", wait)
	}
	if result != "echo:via-"+wait || errMsg != "" {
		t.Errorf("the update was settled as (%q, %q), want (echo:via-%s, no error)", result, errMsg, wait)
	}
	if _, ok := store.resolved[prom]; !ok {
		t.Errorf("the caller's promise was not resolved (resolved=%v rejected=%v)", store.resolved, store.rejected)
	}
}

func TestCSharpAwaitSignalsIsAnUpdateDispatchPoint(t *testing.T) {
	atWait(t, "signals", engine.WithSignalStore(newFakeSignalStore()))
}

func TestCSharpAwaitPromiseIsAnUpdateDispatchPoint(t *testing.T) {
	atWait(t, "promise", engine.WithPromiseStore(&fakePromiseStore{}))
}

func TestCSharpAwaitChildIsAnUpdateDispatchPoint(t *testing.T) {
	atWait(t, "child", engine.WithChildWorkflowStore(newFakeChildStore()), engine.WithWorkflowStartTime(1_000_000))
}
