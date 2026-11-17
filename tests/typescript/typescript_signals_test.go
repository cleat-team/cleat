package typescript

// The signal bindings (cleat#3827, group B): pollSignal and signalWorkflow. A built module run in the in-process engine with a
// SignalStore the test controls, asserting on what the STORE saw as well as what the workflow returned: a workflow that returned
// `undefined` from a poll proves nothing about a store that was never asked.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

type sentRow struct{ target, name, payload string }

// memSignalsE2E is an engine.SignalStore. Deliveries are queued per name (oldest first); DeliveredAtMs stays zero, which the
// engine reads as "visible now". Polling removes nothing and ConsumeSignal does, as in the shipped stores.
type memSignalsE2E struct {
	mu       sync.Mutex
	queue    map[string][]engine.SignalDelivery
	nextID   int64
	sent     []sentRow
	consumed []string
}

func newMemSignalsE2E(seed map[string][]string) *memSignalsE2E {
	m := &memSignalsE2E{queue: map[string][]engine.SignalDelivery{}}
	for name, ps := range seed {
		for _, p := range ps {
			m.nextID++
			m.queue[name] = append(m.queue[name], engine.SignalDelivery{ID: m.nextID, Payload: p})
		}
	}
	return m
}

func (m *memSignalsE2E) DeliverSignal(_ context.Context, target, name, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentRow{target, name, payload})
	m.nextID++
	m.queue[name] = append(m.queue[name], engine.SignalDelivery{ID: m.nextID, Payload: payload})
	return nil
}

func (m *memSignalsE2E) PollSignal(_ context.Context, _, name string) (engine.SignalDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q := m.queue[name]; len(q) > 0 {
		return q[0], true, nil
	}
	return engine.SignalDelivery{}, false, nil
}

func (m *memSignalsE2E) ConsumeSignal(_ context.Context, _ string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, q := range m.queue {
		for i, d := range q {
			if d.ID == id {
				m.queue[name] = append(q[:i], q[i+1:]...)
				m.consumed = append(m.consumed, name)
				return nil
			}
		}
	}
	return nil
}

func (m *memSignalsE2E) PollCancellation(context.Context, string) (bool, string, error) {
	return false, "", nil
}

const signalEntries = "signals_poll,signals_poll_then_wait,signals_send,signals_late"

func buildSignals(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/signals.ts"
	res := build(t, root, src, signalEntries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func TestTypeScriptPollSignalReturnsADeliveredPayloadAndUndefinedWhenThereIsNone(t *testing.T) {
	wasm := buildSignals(t)
	store := newMemSignalsE2E(map[string][]string{"go": {`{"n":7}`}})
	e, closeRT := newEngineWith(t, engine.WithSignalStore(store))
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "signals_poll", json.RawMessage(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("susp=%v err=%v", susp, err)
	}
	// Compared as decoded JSON (the engine sorts keys). `none` must be ABSENT: undefined does not serialise, and a null there
	// would mean the binding handed back something for a name nobody sent.
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	first, _ := got["first"].(map[string]any)
	again, _ := got["again"].(map[string]any)
	if _, hasNone := got["none"]; hasNone || first["n"] != float64(7) || again["n"] != float64(7) {
		t.Fatalf("result %s, want first and again as {n:7} (a poll consumes nothing) and none undefined", raw)
	}
	if len(store.consumed) != 0 {
		t.Errorf("a poll consumed %v; it must not", store.consumed)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeSignalReceived {
			t.Errorf("a poll recorded a %s event; the engine records none for one", ev.EventType)
		}
	}
}

func TestTypeScriptAPolledSignalIsStillThereForTheWaitThatConsumesIt(t *testing.T) {
	wasm := buildSignals(t)
	store := newMemSignalsE2E(map[string][]string{"go": {`"hi"`}})
	e, closeRT := newEngineWith(t, engine.WithSignalStore(store))
	defer closeRT()
	raw, _, _, _, _, err := e.Execute(context.Background(), wasm, "signals_poll_then_wait", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"polled":"hi","waited":{"name":"go","payload":"hi","timedOut":false}}`; raw != want {
		t.Fatalf("result %s, want %s", raw, want)
	}
	if got := strings.Join(store.consumed, ","); got != "go" {
		t.Errorf("consumed %q, want exactly one consumption, by the wait", got)
	}
}

func TestTypeScriptSignalWorkflowIsDeliveredAndRecordedAndNotRedeliveredOnReplay(t *testing.T) {
	wasm := buildSignals(t)
	store := newMemSignalsE2E(nil)
	e, closeRT := newEngineWith(t, engine.WithSignalStore(store), engine.WithWorkflowID("run-self"))
	defer closeRT()
	input := json.RawMessage(`{"target":"run-other"}`)
	raw, history, _, _, _, err := e.Execute(context.Background(), wasm, "signals_send", input)
	if err != nil {
		t.Fatal(err)
	}
	want := []sentRow{{"run-other", "hello", `{"n":1}`}, {"run-other", "bare", `null`}, {"run-self", "to-me", `"text"`}}
	if len(store.sent) != len(want) {
		t.Fatalf("the store saw %d sends %v, want %d", len(store.sent), store.sent, len(want))
	}
	for i, w := range want {
		if store.sent[i] != w {
			t.Errorf("send %d = %+v, want %+v (the payload crosses JSON-encoded; none is null)", i, store.sent[i], w)
		}
	}
	if raw != `{"me":"text"}` {
		t.Errorf("result %s: a string sent to the run's own id did not come back as the same string through pollSignal", raw)
	}
	var recorded int
	for _, ev := range history {
		if ev.EventType == engine.EventTypeSignalReceived {
			recorded++
		}
	}
	if recorded != 3 {
		t.Errorf("%d signal events recorded, want 3", recorded)
	}

	before := len(store.sent)
	if _, _, _, _, _, err := e.Replay(context.Background(), wasm, "signals_send", input, history); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got := len(store.sent); got != before {
		t.Errorf("replay delivered %d signal(s) again; a recorded send is matched, not re-sent", got-before)
	}
}

// What `guarded` on signal_workflow buys, read from the history. The poll beside it is there to MEASURE that it records
// nothing (so leaving it unguarded is correct); it is not a falsification of a guard, and the comment in the plugin says so.
func TestTypeScriptASignalSentAfterASuspensionRecordsNothing(t *testing.T) {
	wasm := buildSignals(t)
	store := newMemSignalsE2E(map[string][]string{"go": {`1`}})
	e, closeRT := newEngineWith(t, engine.WithSignalStore(store))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "signals_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the ordering below is then unmeasured", susp)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeSignalReceived {
			t.Fatalf("the segment recorded a signal event (%s %q) AFTER the call that suspended it; signal_workflow must be guarded", ev.EventType, ev.SignalName)
		}
	}
	if len(store.sent) != 0 {
		t.Fatalf("a send made after the suspension reached the store: %v", store.sent)
	}
}
