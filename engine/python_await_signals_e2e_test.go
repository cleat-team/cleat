//go:build cgo

package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// cleat#3786: a componentized Python guest received the right LENGTHS and NUL bytes from await_signals, and from
// get_scope a NotFound whose "code" was the key's length. Measured on a real worker (v0.5.0 and develop a01afe15):
//
//	signal "go", payload {"approved":true}  ->  name_hex "0000", payload_hex 17 x "00", timed_out False
//	h.poll_signal("go2")                    ->  "second-payload"          (same run: a WIT import returning a string)
//	set_scope("order","o-1"); get_scope()   ->  RuntimeError: CallErrorNotFound (code 3)   (3 == len("o-1"))
//
// The WIT imports took pointers into the guest's memory and returned a packed word of lengths. A component host
// cannot dereference a guest pointer, so the dispatcher wrote into a throwaway slice and returned only the lengths.
// Both imports now return lifted values, and every engine test of them used a mock handler or a pure-Python stub, so
// none could see the bytes (python-sdk/tests uses CleatTestHarness; TestDispatchAwaitSignals asserted only that the
// u64 was 42; python-sdk/examples/all_host_calls_workflow.py runs the TIMED-OUT branch, the one whose result was right).
//
// These tests drive a real component through a real engine and read what the guest was handed. They are separate
// top-level functions on purpose: a t.Run loop skips as subtests while its parent reports PASS (cleat#3437).

const awaitSignalsFixture = "await_signals_outcomes_workflow.py"

// scriptedSignalStore is a SignalStore holding exactly the deliveries a test queues, matched by NAME (the workflow
// id is not what is under test), oldest first, consumed on ConsumeSignal like the real tables.
type scriptedSignalStore struct {
	mu      sync.Mutex
	next    int64
	pending map[string][]SignalDelivery
}

func newScriptedSignalStore() *scriptedSignalStore {
	return &scriptedSignalStore{pending: map[string][]SignalDelivery{}}
}

func (s *scriptedSignalStore) queue(name, payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.pending[name] = append(s.pending[name], SignalDelivery{ID: s.next, Payload: payload})
}

func (s *scriptedSignalStore) DeliverSignal(_ context.Context, _, name, payload string) error {
	s.queue(name, payload)
	return nil
}

func (s *scriptedSignalStore) PollSignal(_ context.Context, _, name string) (SignalDelivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.pending[name]; len(q) > 0 {
		return q[0], true, nil
	}
	return SignalDelivery{}, false, nil
}

func (s *scriptedSignalStore) ConsumeSignal(_ context.Context, _ string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, q := range s.pending {
		for i, d := range q {
			if d.ID == id {
				s.pending[name] = append(q[:i:i], q[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

func (s *scriptedSignalStore) PollCancellation(context.Context, string) (bool, string, error) {
	return false, "", nil
}

type awaitSignalDoc struct {
	Name       string `json:"name"`
	Payload    string `json:"payload"`
	NameHex    string `json:"name_hex"`
	PayloadHex string `json:"payload_hex"`
	TimedOut   bool   `json:"timed_out"`
	Raised     string `json:"raised"`
	Message    string `json:"message"`
}

func decodeAwaitSignal(t *testing.T, result string) awaitSignalDoc {
	t.Helper()
	var d awaitSignalDoc
	if err := json.Unmarshal([]byte(unwrapPythonResult(t, result)), &d); err != nil {
		t.Fatalf("workflow result %q is not the fixture's JSON document: %v", result, err)
	}
	return d
}

// runAwaitSignal delivers one signal to a real component and returns what the guest was handed and the operations it
// made afterwards.
func runAwaitSignal(t *testing.T, name, payload string) (awaitSignalDoc, []string) {
	t.Helper()
	store := newScriptedSignalStore()
	store.queue(name, payload)
	caller := &mockCaller{}
	wasmBytes, eng := pythonEntryEngine(t, awaitSignalsFixture, "await_signal", caller, WithSignalStore(store))
	in := json.RawMessage(`{"signal_name":` + mustJSON(t, name) + `,"timeout_ms":60000}`)
	result, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "await_signal", in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("the run suspended (%s) though the signal was already delivered", suspended.Reason)
	}
	return decodeAwaitSignal(t, result), operationsCalled(caller)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertSignalBytes(t *testing.T, got awaitSignalDoc, wantName, wantPayload string) {
	t.Helper()
	if got.Raised != "" {
		t.Fatalf("the guest raised %s: %s", got.Raised, got.Message)
	}
	if got.NameHex != hex.EncodeToString([]byte(wantName)) {
		t.Errorf("signal name bytes = %q, want %q (hex %s). Measured before the fix: the right LENGTH, every byte 0x00",
			got.NameHex, wantName, hex.EncodeToString([]byte(wantName)))
	}
	if got.PayloadHex != hex.EncodeToString([]byte(wantPayload)) {
		t.Errorf("signal payload bytes (hex) = %q, want %q for %q. Measured before the fix: the right LENGTH, every byte 0x00",
			clipSignalForLog(got.PayloadHex), clipSignalForLog(hex.EncodeToString([]byte(wantPayload))), clipSignalForLog(wantPayload))
	}
	if got.Name != wantName || got.Payload != wantPayload {
		t.Errorf("name/payload = %q/%q, want %q/%q", got.Name, clipSignalForLog(got.Payload), wantName, clipSignalForLog(wantPayload))
	}
	if got.TimedOut {
		t.Errorf("timed_out = true for a signal that was delivered")
	}
}

func clipSignalForLog(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func TestPythonGuestAwaitSignalsReceivesTheDeliveredNameAndPayloadByteForByte(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	got, ops := runAwaitSignal(t, "go", `{"approved":true}`)
	assertSignalBytes(t, got, "go", `{"approved":true}`)
	if len(ops) != 1 {
		t.Errorf("a delivered signal must let the guest run on; operations = %v", ops)
	}
}

func TestPythonGuestAwaitSignalsEmptyPayloadIsEmptyNotZeroBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	got, _ := runAwaitSignal(t, "ping", "")
	assertSignalBytes(t, got, "ping", "")
}

func TestPythonGuestAwaitSignalsMultibytePayloadSurvivesIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const payload = "héllo ✓ 日本語 \U0001F680" // 2-, 3- and 4-byte UTF-8 sequences
	got, _ := runAwaitSignal(t, "vértigo", payload)
	assertSignalBytes(t, got, "vértigo", payload)
}

func TestPythonGuestAwaitSignalsLargePayloadSurvivesIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	payload := strings.Repeat("0123456789abcdef", 3500) // 56,000 bytes: past any small fixed buffer, under the 64 KiB the host reads into
	got, _ := runAwaitSignal(t, "bulk", payload)
	assertSignalBytes(t, got, "bulk", payload)
}

func TestPythonGuestAwaitSignalsFreshWaitStopsTheGuestAtTheWait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	caller := &mockCaller{}
	wasmBytes, eng := pythonEntryEngine(t, awaitSignalsFixture, "await_signal", caller, WithSignalStore(newScriptedSignalStore()))
	_, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "await_signal",
		json.RawMessage(`{"signal_name":"never","timeout_ms":60000}`))
	if err != nil {
		t.Fatalf("Execute: %v -- the run FAILED where it should have suspended", err)
	}
	if suspended == nil {
		t.Fatal("the run completed; with no signal delivered it should have suspended")
	}
	if ops := operationsCalled(caller); len(ops) != 0 {
		t.Errorf("the guest ran on past the pending wait and made %v before the run suspended", ops)
	}
	if len(hist) != 1 || hist[0].EventType != EventTypeAwaitSignals {
		t.Errorf("history = %+v, want exactly the recorded await_signals", hist)
	}
}

func TestPythonGuestAwaitSignalsTimeoutIsATimeoutWithNothingReceived(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	caller := &mockCaller{}
	wasmBytes, eng := pythonEntryEngine(t, awaitSignalsFixture, "await_signal", caller, WithSignalStore(newScriptedSignalStore()))
	ctx := context.Background()
	in := json.RawMessage(`{"signal_name":"never","timeout_ms":60000}`)

	// First segment: nothing is delivered, the wait is recorded, the run suspends.
	_, hist, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "await_signal", in)
	if err != nil || suspended == nil {
		t.Fatalf("first segment: suspended=%v err=%v, want a suspension at the wait", suspended != nil, err)
	}
	// Second segment: the recorded await is moved past its 60 s deadline, as the await-promise test does.
	moved := 0
	for i := range hist {
		if hist[i].EventType == EventTypeAwaitSignals {
			hist[i].TimestampMs -= 61_000
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("expected one await_signals record to move, found %d", moved)
	}
	result, _, suspended, _, _, err := eng.Replay(ctx, wasmBytes, "await_signal", in, hist)
	if err != nil || suspended != nil {
		t.Fatalf("second segment: suspended=%v err=%v, want it to finish on the timeout", suspended != nil, err)
	}
	got := decodeAwaitSignal(t, result)
	if got.Raised != "" || !got.TimedOut || got.Name != "" || got.Payload != "" {
		t.Errorf("timeout: got %+v, want timed_out=True with an empty name and payload", got)
	}
}

type scopeDoc struct {
	Before  []string `json:"before"`
	After   []string `json:"after"`
	Raised  string   `json:"raised"`
	Message string   `json:"message"`
}

func runScopeRoundtrip(t *testing.T, objectType, instanceKey string) scopeDoc {
	t.Helper()
	wasmBytes, eng := pythonEntryEngine(t, awaitSignalsFixture, "scope_roundtrip", &mockCaller{})
	in := json.RawMessage(`{"object_type":` + mustJSON(t, objectType) + `,"instance_key":` + mustJSON(t, instanceKey) + `}`)
	result, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "scope_roundtrip", in)
	if err != nil || suspended != nil {
		t.Fatalf("Execute: suspended=%v err=%v", suspended != nil, err)
	}
	var d scopeDoc
	if err := json.Unmarshal([]byte(unwrapPythonResult(t, result)), &d); err != nil {
		t.Fatalf("workflow result %q is not the fixture's JSON document: %v", result, err)
	}
	return d
}

func TestPythonGuestGetScopeReturnsTheScopeItWasSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	got := runScopeRoundtrip(t, "order", "o-1")
	if got.Raised != "" {
		t.Fatalf("get_scope raised %s: %s (measured before the fix: RuntimeError CallErrorNotFound, code 3 == len(\"o-1\"))", got.Raised, got.Message)
	}
	if len(got.Before) != 2 || got.Before[0] != "" || got.Before[1] != "" {
		t.Errorf("scope before any set_scope = %q, want [\"\", \"\"]", got.Before)
	}
	if len(got.After) != 2 || got.After[0] != "order" || got.After[1] != "o-1" {
		t.Errorf("scope after set_scope(order, o-1) = %q, want [order, o-1]", got.After)
	}
}

func TestPythonGuestGetScopeKeyLengthDoesNotBecomeAnErrorCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	// The engine packed objTypeLen<<32 | instKeyLen; the Python decoder read bits 0-7 as an error code. Any key
	// whose length has a low byte in 1..8 was reported as a CallError of that number, so this is a family, not one case.
	for _, key := range []string{"a", "ab", "abc", "abcdefgh", "abcdefghi"} {
		got := runScopeRoundtrip(t, "t", key)
		if got.Raised != "" {
			t.Errorf("instance key %q (length %d): get_scope raised %s: %s", key, len(key), got.Raised, got.Message)
			continue
		}
		if len(got.After) != 2 || got.After[1] != key {
			t.Errorf("instance key %q: scope after = %q", key, got.After)
		}
	}
}
