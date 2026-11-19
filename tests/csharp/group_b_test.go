//go:build cgo

package csharp

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Group B (cleat#4080, PR 5): signals and promises, driven through the real engine and wasmtime with in-memory stores. The model is
// engine/python_await_promise_outcomes_e2e_test.go: every way a wait can END is its own case, because a value that is right for one
// ending and wrong for another is exactly the defect that file was written about.
//
// Separate top-level functions on purpose: a t.Run loop skips as SUBTESTS while its parent reports PASS, which the skip ledger cannot
// match with a whole-test regex.

// ---- fakes ----------------------------------------------------------------------------------------------------------------------

type fakePromiseStore struct {
	mu                               sync.Mutex
	status, result, errMsg           string // what GetPromise answers: "resolved", "rejected" or "" (pending)
	createErr, resolveErr, rejectErr error
	created                          []struct{ WorkflowID, Name, ID string }
	resolved, rejected               []struct{ ID, Value string }
}

func (f *fakePromiseStore) CreatePromise(_ context.Context, workflowID, name, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, struct{ WorkflowID, Name, ID string }{workflowID, name, id})
	return f.createErr
}

func (f *fakePromiseStore) ResolvePromise(_ context.Context, id, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, struct{ ID, Value string }{id, value})
	return f.resolveErr
}

func (f *fakePromiseStore) RejectPromise(_ context.Context, id, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, struct{ ID, Value string }{id, msg})
	return f.rejectErr
}

func (f *fakePromiseStore) GetPromise(_ context.Context, _, _ string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.result, f.errMsg, nil
}

type deliveredSignal struct{ Target, Name, Payload string }

// fakeSignalStore holds a queue per signal name and records every delivery made through it.
type fakeSignalStore struct {
	mu        sync.Mutex
	next      int64
	queues    map[string][]engine.SignalDelivery
	delivered []deliveredSignal
}

func newFakeSignalStore() *fakeSignalStore {
	return &fakeSignalStore{queues: map[string][]engine.SignalDelivery{}}
}

// pending puts a signal in the queue, as if another workflow had sent it.
func (f *fakeSignalStore) pending(name, payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.queues[name] = append(f.queues[name], engine.SignalDelivery{ID: f.next, Payload: payload})
}

func (f *fakeSignalStore) DeliverSignal(_ context.Context, workflowID, name, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, deliveredSignal{workflowID, name, payload})
	return nil
}

func (f *fakeSignalStore) PollSignal(_ context.Context, _, name string) (engine.SignalDelivery, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.queues[name]; len(q) > 0 {
		return q[0], true, nil
	}
	return engine.SignalDelivery{}, false, nil
}

func (f *fakeSignalStore) ConsumeSignal(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, q := range f.queues {
		for i, d := range q {
			if d.ID == id {
				f.queues[n] = append(q[:i:i], q[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeSignalStore) PollCancellation(context.Context, string) (bool, string, error) {
	return false, "", nil
}

// ---- helpers --------------------------------------------------------------------------------------------------------------------

const promiseIn = `{"promise_id":"p1","timeout_ms":60000}`

// ---- promises -------------------------------------------------------------------------------------------------------------------

func TestCSharpGroupBAwaitPromiseResolvedIsResolvedIncludingTheEmptyValue(t *testing.T) {
	got := mustFinish(t, runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{status: "resolved", result: "hello"})}}))
	if got["result"] != "hello" || got["timed_out"] != false || got["rejected"] != false {
		t.Errorf("resolved \"hello\": got %v", got)
	}
	res := runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{status: "resolved", result: ""})}})
	got = mustFinish(t, res)
	if got["result"] != "" || got["timed_out"] != false || got["rejected"] != false {
		t.Errorf("resolved \"\": got %v, want an empty result that is NOT a timeout", got)
	}
	if ops := opsOf(res); len(ops) != 1 || ops[0] != "svc/after_await" {
		t.Errorf("a settled wait must let the guest run on; operations = %v", ops)
	}
}

func TestCSharpGroupBAwaitPromiseRejectedIsARejectionWithItsReason(t *testing.T) {
	const reason = "boom: the promise was rejected"
	got := mustFinish(t, runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{status: "rejected", errMsg: reason})}}))
	if got["rejected"] != true || got["result"] != reason || got["timed_out"] != false {
		t.Errorf("rejected: got %v, want rejected=true with the reason as the result", got)
	}
}

func TestCSharpGroupBAwaitPromiseHostErrorIsACallExceptionNotAValue(t *testing.T) {
	res := runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn}) // no promise store at all
	got := mustFinish(t, res)
	if got["raised"] == nil || got["raised"] == "" {
		t.Fatalf("a host error reached the guest as a value: %v", got)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on after a host error and made %v", ops)
	}
	t.Logf("a host error surfaces as %v: %v", got["raised"], got["message"])
}

func TestCSharpGroupBAwaitPromiseTimeoutIsATimeoutNotAnEmptyValue(t *testing.T) {
	store := &fakePromiseStore{}
	opts := []engine.EngineOption{engine.WithPromiseStore(store)}
	first := runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn, Options: opts})
	if first.Err != nil || first.Suspended == nil {
		t.Fatalf("first segment: err=%v suspended=%v, want a suspension at the pending wait", first.Err, first.Suspended != nil)
	}
	hist := moveRecordsBack(t, first.History, engine.EventTypeAwaitPromise, 61_000)
	got := mustFinish(t, runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn, Options: opts, Replay: hist}))
	if got["timed_out"] != true || got["result"] != "" || got["rejected"] != false {
		t.Errorf("timeout: got %v, want timed_out=true (an empty value would be indistinguishable from a promise resolved with \"\")", got)
	}
}

func TestCSharpGroupBAwaitPromiseFreshWaitStopsTheGuestAtTheWait(t *testing.T) {
	res := runConformanceEntry(t, "b_await_promise", entryRun{Input: promiseIn,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{})}})
	if res.Err != nil {
		t.Fatalf("the run FAILED where it should have suspended: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("the run completed (%s); with the promise pending it should have suspended", res.Result)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the pending wait and made %v", ops)
	}
	if len(res.History) != 1 || res.History[0].EventType != engine.EventTypeAwaitPromise {
		t.Errorf("history = %+v, want exactly the recorded await_promise", res.History)
	}
}

func TestCSharpGroupBCreatePromiseReturnsTheIdTheStoreWasGiven(t *testing.T) {
	store := &fakePromiseStore{}
	got := mustFinish(t, runConformanceEntry(t, "b_create_promise", entryRun{WorkflowID: "wf-cp", Input: `{"name":"approval","ttl_ms":5000}`,
		Options: []engine.EngineOption{engine.WithPromiseStore(store)}}))
	if len(store.created) != 1 {
		t.Fatalf("the store saw %d CreatePromise calls, want 1", len(store.created))
	}
	c := store.created[0]
	if c.WorkflowID != "wf-cp" || c.Name != "approval" || c.ID == "" {
		t.Errorf("store saw %+v, want workflow wf-cp, name approval and an id", c)
	}
	if got["id"] != c.ID {
		t.Errorf("the guest was returned id %v but the store holds %q", got["id"], c.ID)
	}
}

func TestCSharpGroupBResolvePromiseReachesTheStore(t *testing.T) {
	store := &fakePromiseStore{}
	got := mustFinish(t, runConformanceEntry(t, "b_resolve_promise", entryRun{Input: `{"id":"p9","value":"done"}`,
		Options: []engine.EngineOption{engine.WithPromiseStore(store)}}))
	if got["ok"] != true || len(store.resolved) != 1 || store.resolved[0].ID != "p9" || store.resolved[0].Value != "done" {
		t.Errorf("got %v, store resolved %+v", got, store.resolved)
	}
}

func TestCSharpGroupBRejectPromiseReachesTheStore(t *testing.T) {
	store := &fakePromiseStore{}
	got := mustFinish(t, runConformanceEntry(t, "b_reject_promise", entryRun{Input: `{"id":"p9","error":"nope"}`,
		Options: []engine.EngineOption{engine.WithPromiseStore(store)}}))
	if got["ok"] != true || len(store.rejected) != 1 || store.rejected[0].ID != "p9" || store.rejected[0].Value != "nope" {
		t.Errorf("got %v, store rejected %+v", got, store.rejected)
	}
}

func TestCSharpGroupBResolvePromiseFailureIsACallExceptionNotASilentSuccess(t *testing.T) {
	got := mustFinish(t, runConformanceEntry(t, "b_resolve_promise", entryRun{Input: `{"id":"missing","value":"v"}`,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{resolveErr: engine.ErrPromiseNotFound})}}))
	if got["ok"] == true || got["raised"] == nil {
		t.Fatalf("a store failure was reported as success: %v", got)
	}
	t.Logf("a failed resolve surfaces as %v (code %v): %v", got["raised"], got["code"], got["message"])
}

func TestCSharpGroupBRejectPromiseFailureIsACallExceptionNotASilentSuccess(t *testing.T) {
	got := mustFinish(t, runConformanceEntry(t, "b_reject_promise", entryRun{Input: `{"id":"missing","error":"nope"}`,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{rejectErr: engine.ErrPromiseNotFound})}}))
	if got["ok"] == true || got["raised"] == nil {
		t.Fatalf("a store failure was reported as success: %v", got)
	}
	t.Logf("a failed reject surfaces as %v (code %v): %v", got["raised"], got["code"], got["message"])
}

// ---- signals --------------------------------------------------------------------------------------------------------------------

const signalIn = `{"names":["approve"],"timeout_ms":60000}`

func TestCSharpGroupBAwaitSignalsReceivedCarriesNameAndPayload(t *testing.T) {
	sigs := newFakeSignalStore()
	sigs.pending("approve", `{"by":"alice"}`)
	res := runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: []engine.EngineOption{engine.WithSignalStore(sigs)}})
	got := mustFinish(t, res)
	if got["name"] != "approve" || got["payload"] != `{"by":"alice"}` || got["timed_out"] != false || got["reply_to"] != "" {
		t.Errorf("received: got %v", got)
	}
	if ops := opsOf(res); len(ops) != 1 {
		t.Errorf("a received signal must let the guest run on; operations = %v", ops)
	}
}

func TestCSharpGroupBAwaitSignalsUnwrapsARequestReplyEnvelope(t *testing.T) {
	sigs := newFakeSignalStore()
	sigs.pending("approve", `{"cleat_reply_to":"promise-7","payload":"{\"n\":1}"}`)
	got := mustFinish(t, runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: []engine.EngineOption{engine.WithSignalStore(sigs)}}))
	if got["reply_to"] != "promise-7" || got["payload"] != `{"n":1}` {
		t.Errorf("envelope: got %v, want reply_to=promise-7 and the inner payload", got)
	}
}

func TestCSharpGroupBAwaitSignalsAPayloadThatOnlyMentionsTheKeyIsLeftAlone(t *testing.T) {
	sigs := newFakeSignalStore()
	const raw = `{"cleat_reply_to":"x","payload":"p","extra":"e"}`
	sigs.pending("approve", raw)
	got := mustFinish(t, runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: []engine.EngineOption{engine.WithSignalStore(sigs)}}))
	if got["reply_to"] != "" || got["payload"] != raw {
		t.Errorf("a three-key object was treated as an envelope: %v", got)
	}
}

func TestCSharpGroupBAwaitSignalsTimeoutIsATimeoutNotAnEmptySignal(t *testing.T) {
	sigs := newFakeSignalStore()
	opts := []engine.EngineOption{engine.WithSignalStore(sigs)}
	first := runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: opts})
	if first.Err != nil || first.Suspended == nil {
		t.Fatalf("first segment: err=%v suspended=%v, want a suspension at the wait", first.Err, first.Suspended != nil)
	}
	hist := moveRecordsBack(t, first.History, engine.EventTypeAwaitSignals, 61_000)
	got := mustFinish(t, runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: opts, Replay: hist}))
	if got["timed_out"] != true || got["name"] != "" || got["payload"] != "" {
		t.Errorf("timeout: got %v, want timed_out=true and nothing else", got)
	}
}

func TestCSharpGroupBAwaitSignalsFreshWaitStopsTheGuestAtTheWait(t *testing.T) {
	res := runConformanceEntry(t, "b_await_signals", entryRun{Input: signalIn, Options: []engine.EngineOption{engine.WithSignalStore(newFakeSignalStore())}})
	if res.Err != nil {
		t.Fatalf("the run FAILED where it should have suspended: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("the run completed (%s); with no signal pending it should have suspended", res.Result)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the wait and made %v", ops)
	}
}

// timeout_ms 0 is documented as "wait indefinitely". The host reads a 0ms await_signals as an immediate timed_out (the cleat#1331
// guard), so an SDK that passed 0 through would mean "time out now". With no signal pending the run must SUSPEND at the wait, not
// finish with a timeout.
func TestCSharpGroupBAwaitSignalsZeroTimeoutWaitsIndefinitely(t *testing.T) {
	res := runConformanceEntry(t, "b_await_signals", entryRun{Input: `{"names":["approve"],"timeout_ms":0}`,
		Options: []engine.EngineOption{engine.WithSignalStore(newFakeSignalStore())}})
	if res.Err != nil {
		t.Fatalf("the run FAILED where it should have suspended: %v", res.Err)
	}
	if res.Suspended == nil {
		t.Fatalf("timeout_ms 0 completed (%s): it was passed through as an immediate timeout instead of meaning wait indefinitely", res.Result)
	}
	if ops := opsOf(res); len(ops) != 0 {
		t.Errorf("the guest ran on past the wait and made %v", ops)
	}
}

func TestCSharpGroupBPollSignalFindsAPendingSignalAndReportsNoneAsNotFound(t *testing.T) {
	sigs := newFakeSignalStore()
	sigs.pending("approve", "yes")
	opts := []engine.EngineOption{engine.WithSignalStore(sigs)}
	got := mustFinish(t, runConformanceEntry(t, "b_poll_signal", entryRun{Input: `{"name":"approve"}`, Options: opts}))
	if got["found"] != true || got["payload"] != "yes" {
		t.Errorf("pending: got %v", got)
	}
	got = mustFinish(t, runConformanceEntry(t, "b_poll_signal", entryRun{Input: `{"name":"nothing"}`, Options: opts}))
	if got["found"] != false || got["payload"] != "" {
		t.Errorf("none pending: got %v", got)
	}
}

func TestCSharpGroupBSignalWorkflowDeliversToTheTarget(t *testing.T) {
	sigs := newFakeSignalStore()
	got := mustFinish(t, runConformanceEntry(t, "b_signal_workflow", entryRun{Input: `{"target":"run-42","name":"wake","payload":"{\"k\":1}"}`,
		Options: []engine.EngineOption{engine.WithSignalStore(sigs)}}))
	if got["ok"] != true {
		t.Fatalf("got %v", got)
	}
	if len(sigs.delivered) != 1 || sigs.delivered[0] != (deliveredSignal{"run-42", "wake", `{"k":1}`}) {
		t.Errorf("the store saw %+v, want one delivery to run-42 of wake with the payload", sigs.delivered)
	}
}

// The engine's error path for this call is the authorization check: it answers an error code, which the SDK must raise rather than
// read as success. (A failed DELIVERY is a different matter: the engine logs it and still answers success, cleat#4102, so nothing
// here asserts what a store failure looks like.)
func TestCSharpGroupBSignalWorkflowRefusedByTheAuthCheckIsACallExceptionAndDeliversNothing(t *testing.T) {
	sigs := newFakeSignalStore()
	got := mustFinish(t, runConformanceEntry(t, "b_signal_workflow", entryRun{Input: `{"target":"run-42","name":"wake","payload":"p"}`,
		Options: []engine.EngineOption{
			engine.WithSignalStore(sigs),
			engine.WithRequireSignalAuth(true),
			engine.WithSignalAuthCheck(func(context.Context, string, string) error { return errors.New("not authorised") }),
		}}))
	if got["ok"] == true || got["raised"] == nil {
		t.Fatalf("a refused signal was reported as success: %v", got)
	}
	if len(sigs.delivered) != 0 {
		t.Errorf("a refused signal was delivered anyway: %+v", sigs.delivered)
	}
	t.Logf("a refused signal_workflow surfaces as %v (code %v): %v", got["raised"], got["code"], got["message"])
}
