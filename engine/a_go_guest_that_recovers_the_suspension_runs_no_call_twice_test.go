package engine

// cleat#4285 with a REAL guest, in the one SDK that builds in-tree. The Go SDK stops a guest by panicking cleat.ErrSuspend out of the call that suspended;
// testdata/ignoredsuspend recovers it and makes a service call anyway. Before the host-side refusal that call was dispatched in the first segment, recorded
// after the pending await, and dispatched AGAIN when the workflow resumed: a service call that must happen once, twice. The test measures the thing the bug
// is about, the number of times the service saw the call across both segments, and not a stand-in for it.
//
// Run on the real wasmtime backend against a compiled guest, so the word the host answers is decoded by the SDK's own adapter. One SDK measured; the others
// are covered by the table in a_guest_that_ignores_a_suspension_starts_no_fresh_work_test.go at the host-call level, and Java by its own guest test.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAGoGuestThatRecoversTheChildSuspensionRunsNoCallTwice(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "ignoredsuspend"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-ignored-suspend-go"))
	var childDone atomic.Bool
	eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
		if childDone.Load() {
			return ChildOutcome{Completed: true, Result: `{"r":1}`}, nil
		}
		return ChildOutcome{}, nil
	}}
	dispatched := func() (n int) {
		for _, c := range caller.calls {
			if c.Service == "work" && c.Op == "after_suspend" {
				n++
			}
		}
		return n
	}
	typesOf := func(h []EventRecord) (out []string) {
		for _, r := range h {
			out = append(out, string(r.EventType))
		}
		return out
	}

	// SEGMENT 1: the child is running. The guest is told to stop, recovers, and calls the service.
	_, hist1, susp, _, _, err := eng.Execute(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if susp == nil || !strings.HasPrefix(susp.Reason, "await_child(") {
		t.Fatalf("segment 1 suspension = %+v, want await_child(...): the guest did not reach the await, so nothing below measured the bug", susp)
	}
	if n := dispatched(); n != 0 {
		t.Errorf("segment 1: the service saw the call %d time(s) from a guest that had already been told to stop; want 0 (dispatched=%v)", n, opsOf(caller.calls))
	}
	if got := typesOf(hist1); len(got) != 1 || got[0] != string(EventTypeAwaitChild) {
		t.Errorf("segment 1 history = %v, want just the pending await_child: a refused call records nothing", got)
	}

	// SEGMENT 2: the child has finished, and the workflow resumes by replaying that history. The call is made, once.
	childDone.Store(true)
	out, hist2, susp2, _, _, err := eng.Replay(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`), hist1)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if susp2 != nil || !strings.Contains(out, "ok") {
		t.Fatalf("segment 2 did not complete: result=%q suspended=%+v history=%v", out, susp2, typesOf(hist2))
	}
	if n := dispatched(); n != 1 {
		t.Errorf("the service saw the call %d times across both segments, want exactly 1 (dispatched=%v)", n, opsOf(caller.calls))
	}
	calls := 0
	for _, r := range hist2 {
		if r.EventType == EventTypeCall {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("final history holds %d call records, want 1: %v", calls, typesOf(hist2))
	}
}
