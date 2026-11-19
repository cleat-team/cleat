//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Group E (cleat#4080, PR 5): update handlers and queryable state, driven through the real engine and wasmtime with an in-memory
// update store. The model is engine/updater.go and python-sdk's dispatch_updates.
//
// Separate top-level functions on purpose, as in groups B and C.

// fakeUpdateStore queues update requests and records how each was settled. Delivery does not consume a request (the engine's contract):
// only completing it moves it out of pending, which is what lets a stopped handler's update be redelivered.
type fakeUpdateStore struct {
	mu        sync.Mutex
	requests  []engine.UpdateRequestInfo
	completed map[string]bool      // by request id
	outcomes  map[string][2]string // request id -> {result, error}
	resolved  map[string]string    // promise id -> value
	rejected  map[string]string    // promise id -> reason
}

func newFakeUpdateStore() *fakeUpdateStore {
	return &fakeUpdateStore{completed: map[string]bool{}, outcomes: map[string][2]string{}, resolved: map[string]string{}, rejected: map[string]string{}}
}

// add queues a request; its id and promise id are derived from its position so a test can name them.
func (f *fakeUpdateStore) add(name, payload string) (requestID, promiseID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.requests) + 1
	requestID, promiseID = "req-"+strconv.Itoa(n), "prom-"+strconv.Itoa(n)
	f.requests = append(f.requests, engine.UpdateRequestInfo{RequestID: requestID, UpdateName: name, Payload: payload, PromiseID: promiseID, Status: "pending"})
	return requestID, promiseID
}

func (f *fakeUpdateStore) GetPendingUpdateRequests(_ context.Context, _ string) ([]engine.UpdateRequestInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []engine.UpdateRequestInfo
	for _, r := range f.requests {
		if !f.completed[r.RequestID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeUpdateStore) CompleteUpdateRequest(_ context.Context, _, requestID, result, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed[requestID] = true
	f.outcomes[requestID] = [2]string{result, errMsg}
	return nil
}

func (f *fakeUpdateStore) ResolvePromise(_ context.Context, promiseID, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved[promiseID] = value
	return nil
}

func (f *fakeUpdateStore) RejectPromise(_ context.Context, promiseID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[promiseID] = reason
	return nil
}

func withUpdates(s *fakeUpdateStore) []engine.EngineOption {
	return []engine.EngineOption{engine.WithUpdateStore(s)}
}

func (f *fakeUpdateStore) outcome(requestID string) (result, errMsg string, done bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.outcomes[requestID]
	return o[0], o[1], ok
}

// ---- serving updates ------------------------------------------------------------------------------------------------------------

func TestCSharpGroupEDispatchServesEveryEndingAndSettlesEachCallersPromise(t *testing.T) {
	store := newFakeUpdateStore()
	okReq, okProm := store.add("echo", "hello")
	emptyReq, emptyProm := store.add("empty", "x")
	badReq, badProm := store.add("strict", "bad")
	boomReq, boomProm := store.add("boom", "")
	ghostReq, ghostProm := store.add("ghost", "")
	res := runConformanceEntry(t, "e_dispatch", entryRun{Options: withUpdates(store)})
	got := mustFinish(t, res)
	if got["ok"] != true {
		t.Fatalf("the dispatch entry did not finish normally: %v", got)
	}
	if ops := opsOf(res); len(ops) != 1 || ops[0] != "svc/after_dispatch" {
		t.Errorf("the guest must run on after serving; operations = %v", ops)
	}
	type want struct {
		req, prom, result, errContains string
	}
	for _, w := range []want{
		{okReq, okProm, "echo:hello", ""},
		{emptyReq, emptyProm, "", ""}, // an empty result with no error RESOLVES: it is an outcome, not a missing one
		{badReq, badProm, "", "failed validation"},
		{boomReq, boomProm, "", "boom!"},
		{ghostReq, ghostProm, "", "no update handler registered for 'ghost'"},
	} {
		result, errMsg, done := store.outcome(w.req)
		if !done {
			t.Errorf("%s was never completed: the caller would hold a promise nothing settles", w.req)
			continue
		}
		if w.errContains == "" {
			if errMsg != "" || result != w.result {
				t.Errorf("%s: result=%q error=%q, want result %q and no error", w.req, result, errMsg, w.result)
			}
			if _, ok := store.resolved[w.prom]; !ok {
				t.Errorf("%s: promise %s was not resolved (resolved=%v rejected=%v)", w.req, w.prom, store.resolved, store.rejected)
			}
			continue
		}
		if errMsg == "" || !contains(errMsg, w.errContains) {
			t.Errorf("%s: error=%q, want it to contain %q", w.req, errMsg, w.errContains)
		}
		if _, ok := store.rejected[w.prom]; !ok {
			t.Errorf("%s: promise %s was not rejected (resolved=%v rejected=%v)", w.req, w.prom, store.resolved, store.rejected)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestCSharpGroupEAHandlerThatSleepsStopsTheRunAndCompletesNothing(t *testing.T) {
	store := newFakeUpdateStore()
	req, prom := store.add("naps", "")
	res := runConformanceEntry(t, "e_dispatch", entryRun{Options: withUpdates(store)})
	if res.Err != nil {
		t.Fatalf("the run FAILED where it should have suspended: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("the run completed with %q; the handler's hour-long sleep should have suspended it", res.Result)
	}
	if _, _, done := store.outcome(req); done {
		t.Errorf("a stopped handler's update was completed: the caller was answered for work the host only deferred")
	}
	if _, ok := store.resolved[prom]; ok {
		t.Errorf("a stopped handler's promise was resolved")
	}
	if _, ok := store.rejected[prom]; ok {
		t.Errorf("a stopped handler's promise was rejected with a message")
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the stop and made %v", ops)
	}
}

func TestCSharpGroupEAHandlersOwnServiceCallsAreMadeAndRecorded(t *testing.T) {
	store := newFakeUpdateStore()
	req, _ := store.add("calls", "")
	res := runConformanceEntry(t, "e_dispatch", entryRun{Options: withUpdates(store)})
	mustFinish(t, res)
	if ops := opsOf(res); len(ops) != 2 || ops[0] != "svc/from_handler" || ops[1] != "svc/after_dispatch" {
		t.Errorf("operations = %v, want the handler's own call, then the entry's", ops)
	}
	if result, errMsg, done := store.outcome(req); !done || result != "called" || errMsg != "" {
		t.Errorf("the update's outcome = (%q, %q, done=%v), want (called, , true)", result, errMsg, done)
	}
}

func TestCSharpGroupENoPendingUpdatesIsANoOp(t *testing.T) {
	store := newFakeUpdateStore()
	res := runConformanceEntry(t, "e_dispatch", entryRun{Options: withUpdates(store)})
	mustFinish(t, res)
	if len(store.outcomes) != 0 {
		t.Errorf("nothing was pending but %d outcomes were recorded", len(store.outcomes))
	}
}

func TestCSharpGroupESleepIsADispatchPoint(t *testing.T) {
	store := newFakeUpdateStore()
	req, prom := store.add("echo", "via-sleep")
	res := runConformanceEntry(t, "e_sleep_serves_updates", entryRun{Options: withUpdates(store), WorkflowID: "wf-e"})
	if res.Err != nil {
		t.Fatalf("the run failed: %v", res.Err)
	}
	result, errMsg, done := store.outcome(req)
	if !done || result != "echo:via-sleep" || errMsg != "" {
		t.Fatalf("the update pending at the sleep was not served: outcome=(%q, %q, done=%v) suspended=%v", result, errMsg, done, res.Suspended != nil)
	}
	if _, ok := store.resolved[prom]; !ok {
		t.Errorf("the caller's promise was not resolved")
	}
}

// ---- the low-level calls --------------------------------------------------------------------------------------------------------

func TestCSharpGroupEPollUpdateReturnsTheEnvelopeAndEmptyWhenNothingIsPending(t *testing.T) {
	store := newFakeUpdateStore()
	empty := mustFinish(t, runConformanceEntry(t, "e_poll_update", entryRun{Options: withUpdates(store)}))
	if empty["envelope"] != "" {
		t.Errorf("nothing pending, but poll returned %v", empty["envelope"])
	}
	store.add("echo", `{"a":1}`)
	got := mustFinish(t, runConformanceEntry(t, "e_poll_update", entryRun{Options: withUpdates(store)}))
	var env struct {
		Name      string `json:"name"`
		Payload   string `json:"payload"`
		RequestID string `json:"request_id"`
	}
	raw, _ := got["envelope"].(string)
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("poll did not return an envelope: %q: %v", raw, err)
	}
	if env.Name != "echo" || env.Payload != `{"a":1}` || env.RequestID == "" {
		t.Errorf("envelope = %+v, want name echo, the payload verbatim and a non-empty request id", env)
	}
}

func TestCSharpGroupECompleteUpdateSettlesAndAnEmptyResultStillResolves(t *testing.T) {
	store := newFakeUpdateStore()
	store.add("echo", "x")
	// The request id completion takes is the host's composite key from a poll; read it from one.
	polled := mustFinish(t, runConformanceEntry(t, "e_poll_update", entryRun{Options: withUpdates(store)}))
	var env struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(polled["envelope"].(string)), &env); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"request_id": env.RequestID, "result": "", "error": ""})
	got := mustFinish(t, runConformanceEntry(t, "e_complete_update", entryRun{Input: string(in), Options: withUpdates(store)}))
	if got["raised"] != nil {
		t.Fatalf("completing a delivered update raised: %v", got)
	}
	if result, errMsg, done := store.outcome("req-1"); !done || result != "" || errMsg != "" {
		t.Errorf("outcome = (%q, %q, done=%v), want an empty resolved completion", result, errMsg, done)
	}
	if _, ok := store.resolved["prom-1"]; !ok {
		t.Errorf("an empty result with no error must RESOLVE the caller's promise; resolved=%v rejected=%v", store.resolved, store.rejected)
	}
}

// ---- query state ----------------------------------------------------------------------------------------------------------------

func TestCSharpGroupESetQueryStateReachesTheHost(t *testing.T) {
	res := runConformanceEntry(t, "e_set_query_state", entryRun{Input: `{"key":"progress","value":"{\"pct\":40}"}`})
	got := mustFinish(t, res)
	if got["ok"] != true {
		t.Fatalf("set_query_state did not finish normally: %v", got)
	}
	if res.QueryState["progress"] != `{"pct":40}` {
		t.Errorf("the engine reports query state %v, want progress={\"pct\":40} exactly as the guest set it", res.QueryState)
	}
}
