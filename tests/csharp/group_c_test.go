//go:build cgo

package csharp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Group C (cleat#4080, PR 5): child workflows, driven through the real engine and wasmtime with an in-memory child store. The models are
// engine/python_pending_child_await_e2e_test.go and engine/python_child_poll_e2e_test.go.
//
// Separate top-level functions on purpose, as in group B: a t.Run loop skips as subtests while its parent reports PASS.

type startedChild struct {
	Parent, Name, Input, Policy string
	Version, Priority           int
	RunID                       string
}

// fakeChildStore starts children under ids it makes up and answers GetChildResult from a map the test fills in.
type fakeChildStore struct {
	mu       sync.Mutex
	next     int
	started  []startedChild
	outcomes map[string]engine.ChildOutcome // by run id; a missing id is "still running"
	doneAtMs map[string]int64               // completion timestamps: poll_child refuses a finished child that has none, for determinism
	startErr error
}

func newFakeChildStore() *fakeChildStore {
	return &fakeChildStore{outcomes: map[string]engine.ChildOutcome{}, doneAtMs: map[string]int64{}}
}

func (f *fakeChildStore) record(parent, name, input string, version int, policy string, priority int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.next++
	id := fmt.Sprintf("child-%d", f.next)
	f.started = append(f.started, startedChild{parent, name, input, policy, version, priority, id})
	return id, nil
}

func (f *fakeChildStore) StartChildWorkflow(_ context.Context, parent, name, input string, version int, policy string, priority int) (string, error) {
	return f.record(parent, name, input, version, policy, priority)
}

func (f *fakeChildStore) StartChildWorkflowAtomic(_ context.Context, _, parent, name, input string, version int, policy string, _ engine.EventRecord, priority int) (string, error) {
	return f.record(parent, name, input, version, policy, priority)
}

func (f *fakeChildStore) GetChildResult(_ context.Context, runID string) (engine.ChildOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outcomes[runID], nil
}

func (f *fakeChildStore) GetChildCompletedAtMs(_ context.Context, runID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ms, ok := f.doneAtMs[runID]
	return ms, ok, nil
}

func (f *fakeChildStore) ResolveVersionByTag(context.Context, string, string) (int, error) {
	return 0, nil
}

func (f *fakeChildStore) finish(runID, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes[runID] = engine.ChildOutcome{Completed: true, Result: result}
	f.doneAtMs[runID] = 1 // long before the parent's clock (withChildren), so a poll answers "completed"
}

func (f *fakeChildStore) fail(runID, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes[runID] = engine.ChildOutcome{Completed: true, Failed: true, Error: message}
	f.doneAtMs[runID] = 1
}

// The parent's durable clock is set because a fresh execution otherwise takes the engine's cached wall clock, which is ZERO in a test
// process, and poll_child then answers "running" for a child completed at any positive time (completedAtMs > nowMs): the replay-safety
// rule in engine/children.go. engine/python_child_poll_e2e_test.go does the same.
func withChildren(s *fakeChildStore) []engine.EngineOption {
	return []engine.EngineOption{engine.WithChildWorkflowStore(s), engine.WithWorkflowStartTime(1_000_000)}
}

// ---- starting a child -----------------------------------------------------------------------------------------------------------

func TestCSharpGroupCStartChildReturnsTheStoresRunIdAndPassesNameAndInput(t *testing.T) {
	store := newFakeChildStore()
	got := mustFinish(t, runConformanceEntry(t, "c_child_workflow", entryRun{WorkflowID: "wf-parent", Input: `{"name":"charge","input":"{\"amount\":5}"}`, Options: withChildren(store)}))
	if len(store.started) != 1 {
		t.Fatalf("the store saw %d starts, want 1 (result %v)", len(store.started), got)
	}
	c := store.started[0]
	if got["run_id"] != c.RunID || c.Parent != "wf-parent" || c.Name != "charge" || c.Input != `{"amount":5}` {
		t.Errorf("guest got %v; store saw %+v, want the same run id, parent wf-parent, name charge, the input verbatim", got, c)
	}
}

func TestCSharpGroupCStartChildHostFailureIsACallExceptionNotAValue(t *testing.T) {
	store := newFakeChildStore()
	store.startErr = errors.New("no capacity")
	got := mustFinish(t, runConformanceEntry(t, "c_child_workflow", entryRun{Input: `{"name":"charge","input":"{}"}`, Options: withChildren(store)}))
	if got["raised"] == nil || got["run_id"] != nil {
		t.Fatalf("a refused start reached the guest as a value: %v", got)
	}
	t.Logf("a refused child start surfaces as %v: %v", got["raised"], got["message"])
}

// The policy goes over the wire in the engine's canonical spelling whichever way the caller named it: the engine matches EXACTLY and in
// upper case, so a lower-case spelling is refused at the write boundary.
func TestCSharpGroupCStartWithOptionsSendsVersionPriorityAndTheCanonicalPolicy(t *testing.T) {
	cases := []struct{ in, want string }{
		{"default", ""}, {"abandon", "ABANDON"}, {"terminate", "TERMINATE"}, {"request_cancel", "REQUEST_CANCEL"},
	}
	for _, c := range cases {
		store := newFakeChildStore()
		input := fmt.Sprintf(`{"name":"charge","input":"{}","version":3,"priority":7,"policy":%q}`, c.in)
		got := mustFinish(t, runConformanceEntry(t, "c_child_with_options", entryRun{Input: input, Options: withChildren(store)}))
		if len(store.started) != 1 {
			t.Fatalf("policy %q: the store saw %d starts, want 1 (result %v)", c.in, len(store.started), got)
		}
		s := store.started[0]
		if s.Policy != c.want || s.Version != 3 || s.Priority != 7 {
			t.Errorf("policy %q: store saw %+v, want policy %q, version 3, priority 7", c.in, s, c.want)
		}
	}
}

// ---- awaiting -------------------------------------------------------------------------------------------------------------------

func TestCSharpGroupCAwaitChildCompletedReturnsTheAnswerAndLetsTheGuestRunOn(t *testing.T) {
	store := newFakeChildStore()
	store.finish("child-1", `{"paid":true}`)
	res := runConformanceEntry(t, "c_await_child", entryRun{Input: `{"run_id":"child-1"}`, Options: withChildren(store)})
	got := mustFinish(t, res)
	if got["answer"] != `{"paid":true}` {
		t.Errorf("await_child answered %v, want the child's result", got["answer"])
	}
	if ops := opsOf(res); len(ops) != 1 || ops[0] != "svc/after_await" {
		t.Errorf("a finished child must let the guest run on; operations = %v", ops)
	}
}

func TestCSharpGroupCAwaitChildPendingStopsTheGuestBeforeItsNextCall(t *testing.T) {
	res := runConformanceEntry(t, "c_await_child", entryRun{Input: `{"run_id":"child-1"}`, Options: withChildren(newFakeChildStore())})
	if res.Err != nil {
		t.Fatalf("the run FAILED where it should have suspended: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("the run completed with %q; with the child unfinished it should have suspended", res.Result)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the pending await and made %v: the stop must unwind it at the call", ops)
	}
}

func TestCSharpGroupCAwaitAllChildrenReturnsEveryResultInOrder(t *testing.T) {
	store := newFakeChildStore()
	store.finish("child-1", "one")
	store.finish("child-2", "two")
	res := runConformanceEntry(t, "c_await_all_children", entryRun{Input: `{"run_ids":["child-1","child-2"]}`, Options: withChildren(store)})
	got := mustFinish(t, res)
	results, _ := got["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want two", got)
	}
	for i, want := range []struct{ id, result string }{{"child-1", "one"}, {"child-2", "two"}} {
		r, _ := results[i].(map[string]any)
		if r["run_id"] != want.id || r["result"] != want.result || r["error"] != nil {
			t.Errorf("result %d = %v, want run_id %s result %s and no error", i, r, want.id, want.result)
		}
	}
}

func TestCSharpGroupCAwaitAllChildrenPendingStopsTheGuest(t *testing.T) {
	store := newFakeChildStore()
	store.finish("child-1", "one") // child-2 is still running
	res := runConformanceEntry(t, "c_await_all_children", entryRun{Input: `{"run_ids":["child-1","child-2"]}`, Options: withChildren(store)})
	if res.Err != nil || res.Suspended == nil {
		t.Fatalf("err=%v suspended=%v, want a clean suspension while one child is unfinished (result %q)", res.Err, res.Suspended != nil, res.Result)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the pending await and made %v", ops)
	}
}

func TestCSharpGroupCAwaitAnyChildReturnsTheFinishedOne(t *testing.T) {
	store := newFakeChildStore()
	store.finish("child-2", "second won")
	got := mustFinish(t, runConformanceEntry(t, "c_await_any_child", entryRun{Input: `{"run_ids":["child-1","child-2"]}`, Options: withChildren(store)}))
	if got["run_id"] != "child-2" || got["result"] != "second won" || got["error"] != nil {
		t.Errorf("await_any_child = %v, want child-2 with its result and no error", got)
	}
}

func TestCSharpGroupCAwaitAnyChildAFailedWinnerIsAWinnerNotAHostError(t *testing.T) {
	store := newFakeChildStore()
	store.fail("child-1", "child blew up")
	got := mustFinish(t, runConformanceEntry(t, "c_await_any_child", entryRun{Input: `{"run_ids":["child-1"]}`, Options: withChildren(store)}))
	if got["raised"] != nil {
		t.Fatalf("a child that failed was raised as a host error: %v", got)
	}
	if got["run_id"] != "child-1" || got["error"] != "child blew up" {
		t.Errorf("await_any_child = %v, want child-1 carrying its error", got)
	}
}

// ---- polling --------------------------------------------------------------------------------------------------------------------

func TestCSharpGroupCPollChildIsNotFoundWhileRunningAndFoundWhenFinished(t *testing.T) {
	store := newFakeChildStore()
	running := mustFinish(t, runConformanceEntry(t, "c_poll_child", entryRun{Input: `{"run_id":"child-1"}`, Options: withChildren(store)}))
	if running["found"] != false {
		t.Errorf("a running child polled as %v, want found=false", running)
	}
	store.finish("child-1", "done")
	finished := mustFinish(t, runConformanceEntry(t, "c_poll_child", entryRun{Input: `{"run_id":"child-1"}`, Options: withChildren(store)}))
	if finished["found"] != true || finished["run_id"] != "child-1" || finished["result"] != "done" || finished["error"] != nil {
		t.Errorf("a finished child polled as %v, want found with run_id child-1, result done, no error", finished)
	}
}

func TestCSharpGroupCPollChildAFailedChildCarriesItsError(t *testing.T) {
	store := newFakeChildStore()
	store.fail("child-1", "it broke")
	got := mustFinish(t, runConformanceEntry(t, "c_poll_child", entryRun{Input: `{"run_id":"child-1"}`, Options: withChildren(store)}))
	if got["found"] != true || got["error"] != "it broke" {
		t.Errorf("a failed child polled as %v, want found with its error", got)
	}
}
