package typescript

// The workflow update bindings (cleat#3827, group A): register a handler, receive a delivery, complete the request.
//
// WHAT THIS HOLDS. A built module run in the in-process engine, with an UpdateStore the test controls. Without a store the
// engine reports "nothing pending" and says so to nobody (engine/updater.go:98), so every test here starts from a store that
// has requests in it and asserts on what the STORE saw, not only on what the workflow returned: a workflow that returned the
// right total without any update having been delivered is the failure this file exists to rule out.
//
// TWO THINGS THE TEST MUST NOT BE FOOLED BY. (1) cleat-sdk dispatches updates itself inside sleep, await_signals and the other
// waiting calls, and with no Rust handler it answers an update "no update handler registered". The guest's own dispatch runs
// first, so in the ordinary case it has already taken every pending update and the SDK's finds nothing -- which is why the
// plugin's switch (set_external_dispatch) is invisible to most tests. It matters when a handler is RUNNING: its own sleep skips
// the guest's dispatch (re-entrancy guard) but not the SDK's, which would find the still-pending request and answer it with
// that error. TestTypeScriptAnUpdateHandlerThatSuspendsLeavesTheRequestPending is the test that fails without the switch
// (measured: "the request was completed although its handler had not finished"). (2) The handler's side effect (`total`) is
// only visible through its own result, so the results are asserted as a SEQUENCE: a refused update that wrongly ran its handler
// would show in the next total.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

type memUpdateRow struct {
	info      engine.UpdateRequestInfo
	completed bool
	result    string
	errMsg    string
}

// memUpdates is an in-memory engine.UpdateStore. Delivery does not consume a row; completion does, as in the shipped stores.
type memUpdates struct {
	mu       sync.Mutex
	rows     []*memUpdateRow
	complete []string // request ids, in completion order
	settled  []string // "<promiseID>:resolved:<value>" or "<promiseID>:rejected:<message>"
}

func newMemUpdates(reqs ...[2]string) *memUpdates { // each: {name, payload-JSON-text}
	m := &memUpdates{}
	for i, r := range reqs {
		m.rows = append(m.rows, &memUpdateRow{info: engine.UpdateRequestInfo{
			RequestID: fmt.Sprintf("req-%d", i+1), UpdateName: r[0], Payload: r[1],
			PromiseID: fmt.Sprintf("prom-%d", i+1), Status: "pending",
		}})
	}
	return m
}

func (m *memUpdates) GetPendingUpdateRequests(_ context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.UpdateRequestInfo
	for _, r := range m.rows {
		if !r.completed {
			info := r.info
			info.WorkflowID = workflowID
			out = append(out, info)
		}
	}
	return out, nil
}

func (m *memUpdates) CompleteUpdateRequest(_ context.Context, _, requestID, result, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.info.RequestID == requestID {
			r.completed, r.result, r.errMsg = true, result, errMsg
			m.complete = append(m.complete, requestID)
			return nil
		}
	}
	return fmt.Errorf("unknown request id %q", requestID)
}

func (m *memUpdates) ResolvePromise(_ context.Context, id, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, id+":resolved:"+value)
	return nil
}

func (m *memUpdates) RejectPromise(_ context.Context, id, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, id+":rejected:"+msg)
	return nil
}

func (m *memUpdates) row(i int) memUpdateRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.rows[i]
}

func (m *memUpdates) pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if !r.completed {
			n++
		}
	}
	return n
}

func buildUpdates(t *testing.T, entries string) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/updates.ts"
	res := build(t, root, src, entries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func eventTypes(history []engine.EventRecord) []string {
	var out []string
	for _, ev := range history {
		out = append(out, string(ev.EventType))
	}
	return out
}

func TestTypeScriptUpdatesAreAnsweredByTheGuestsHandlers(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	store := newMemUpdates(
		[2]string{"add", `{"n":5}`},
		[2]string{"add", `{"n":7}`},
		[2]string{"boom", `{}`},
		[2]string{"nope", `{}`},
		[2]string{"add", `{"n":"x"}`}, // refused by the validator: the handler must not run
		[2]string{"add", `{"n":3}`},   // 12 + 3: a wrongly-run refused handler would make this something else
		[2]string{"quiet", `{}`},
	)
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(store))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "updates", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	// The precondition: the segment suspended at the sleep AFTER the dispatch point, so every update was delivered before it.
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the updates were delivered at the wrong point or not at all", susp)
	}
	if n := store.pending(); n != 0 {
		t.Fatalf("%d request(s) still pending: the SDK delivered a subset, or none", n)
	}

	want := []struct {
		result, err string // the completion's result JSON text, or its error message
	}{
		{`{"total":5}`, ""},
		{`{"total":12}`, ""},
		{"", "boom"},
		{"", `cleat: no update handler registered for "nope"`},
		{"", "n must be a number"},
		{`{"total":15}`, ""},
		{`null`, ""},
	}
	for i, w := range want {
		r := store.row(i)
		if !r.completed {
			t.Errorf("request %d (%s) was never completed", i+1, r.info.UpdateName)
			continue
		}
		if r.result != w.result || !strings.Contains(r.errMsg, w.err) || (w.err == "" && r.errMsg != "") {
			t.Errorf("request %d (%s): completed with result %q, error %q; want result %q, error containing %q", i+1, r.info.UpdateName, r.result, r.errMsg, w.result, w.err)
		}
	}
	// The completions are in delivery order, and each settled its own caller's promise the way the result says.
	if got := strings.Join(store.complete, ","); got != "req-1,req-2,req-3,req-4,req-5,req-6,req-7" {
		t.Errorf("completion order %s, want delivery order", got)
	}
	if len(store.settled) != 7 || store.settled[0] != `prom-1:resolved:{"total":5}` || !strings.HasPrefix(store.settled[2], "prom-3:rejected:boom") {
		t.Errorf("the callers' promises were settled as %v", store.settled)
	}

	// The history brackets each handler: received, then completed, and the three registrations come first.
	got := strings.Join(eventTypes(history), " ")
	if !strings.HasPrefix(got, "update_handler update_handler update_handler update_received update_completed update_received update_completed") {
		t.Errorf("history = %s\nwant three registrations, then received/completed pairs", got)
	}
}

// Replay re-runs the handlers (that is what rebuilds the state they changed) but must not complete a request a second time,
// and must reach the same suspension.
func TestTypeScriptUpdatesReplayWithoutCompletingTwice(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	store := newMemUpdates([2]string{"add", `{"n":5}`}, [2]string{"add", `{"n":7}`})
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(store))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "updates", json.RawMessage(`{}`))
	if err != nil || susp == nil {
		t.Fatalf("first pass: susp=%v err=%v", susp, err)
	}
	before := len(store.complete)
	_, history2, susp2, _, _, err := e.Replay(context.Background(), wasm, "updates", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if susp2 == nil || !strings.Contains(susp2.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the replay did not reach the same suspension: %v", susp2)
	}
	if got := len(store.complete); got != before {
		t.Errorf("replay completed %d request(s) again (%d -> %d); the originals were settled and re-settling is an error", got-before, before, got)
	}
	if a, b := strings.Join(eventTypes(history), " "), strings.Join(eventTypes(history2), " "); a != b {
		t.Errorf("replay recorded a different history:\n first: %s\n replay: %s", a, b)
	}
}

// A handler that suspends mid-body: the delivery is recorded and the request stays pending, so the next segment redelivers.
func TestTypeScriptAnUpdateHandlerThatSuspendsLeavesTheRequestPending(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	store := newMemUpdates([2]string{"slow", `{}`})
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(store))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "updates_suspending", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended at %v, want the handler's own sleep (60000ms), not the workflow's (120000ms)", susp)
	}
	if store.pending() != 1 {
		t.Fatalf("the request was completed (error %q) although its handler had not finished; something other than the handler answered it", store.row(0).errMsg)
	}
	var sawReceived, sawCompleted bool
	for _, ev := range history {
		sawReceived = sawReceived || ev.EventType == engine.EventTypeUpdateReceived
		sawCompleted = sawCompleted || ev.EventType == engine.EventTypeUpdateCompleted
	}
	if !sawReceived || sawCompleted {
		t.Errorf("history has received=%v completed=%v, want received only", sawReceived, sawCompleted)
	}
}

// With no handler registered an update is still ANSWERED, and the caller hears an error rather than waiting forever.
func TestTypeScriptAnUpdateWithNoHandlerIsAnsweredWithAnError(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	store := newMemUpdates([2]string{"anything", `{}`})
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(store))
	defer closeRT()
	if _, _, _, _, _, err := e.Execute(context.Background(), wasm, "updates_no_handler", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if r := store.row(0); !r.completed || !strings.Contains(r.errMsg, `no update handler registered for "anything"`) {
		t.Fatalf("the request was left as completed=%v error=%q", r.completed, r.errMsg)
	}
}

// dispatchUpdates() handles an update where the workflow says, with no suspension before it.
func TestTypeScriptDispatchUpdatesIsAnExplicitDispatchPoint(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	store := newMemUpdates([2]string{"add", `{"n":4}`}, [2]string{"add", `{"n":6}`})
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(store))
	defer closeRT()
	raw, _, susp, _, _, err := e.Execute(context.Background(), wasm, "updates_explicit", json.RawMessage(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("the workflow should have finished: susp=%v err=%v", susp, err)
	}
	if raw != `{"total":10}` {
		t.Fatalf("result %s, want {\"total\":10}: the handlers ran in the workflow's own closure", raw)
	}
	if store.pending() != 0 {
		t.Fatalf("%d request(s) left pending", store.pending())
	}
}

// A workflow that is never sent an update records nothing for it: the poll before each suspension point is not an event.
func TestTypeScriptAWorkflowThatReceivesNoUpdateRecordsNoUpdateEvent(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(newMemUpdates()))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "updates", json.RawMessage(`{}`))
	if err != nil || susp == nil {
		t.Fatalf("susp=%v err=%v", susp, err)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeUpdateReceived || ev.EventType == engine.EventTypeUpdateCompleted {
			t.Fatalf("an update event %s was recorded for a workflow nobody sent an update to", ev.EventType)
		}
	}
}

// What `guarded` on register_update_handler buys, read from the history and not from the return value (the shape of
// TestTypeScriptACallAfterASuspensionRecordsNothing). NOT COVERED, and said so: the same guard on poll_update and
// complete_update, because showing it needs a request that is still pending AFTER the segment has suspended, and the only
// suspending calls are themselves dispatch points that would have delivered it first.
func TestTypeScriptARegistrationAfterASuspensionRecordsNothing(t *testing.T) {
	wasm := buildUpdates(t, "updates,updates_suspending,updates_no_handler,updates_explicit,updates_late")
	e, closeRT := newEngineWith(t, engine.WithUpdateStore(newMemUpdates()))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "updates_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the ordering below is then unmeasured", susp)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeUpdateHandler {
			t.Fatalf("the segment recorded a handler registration (%+v) AFTER the call that suspended it; register_update_handler must be guarded", ev)
		}
	}
}
