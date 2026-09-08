package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The fresh path and the replay arm parsed the SAME field two different ways.
//
// Every SDK sends the names as a JSON array -- crates/cleat-sdk/src/host_calls.rs
// serde_json's them "matching Go's adapter.go behavior", and the AssemblyScript
// and Python wrappers document a JSON array string. The replay arm honours that
// (engine/signaller.go json.Unmarshals, falling back to splitSignalNames), but
// the fresh path called splitSignalNames alone, which splits on commas only:
//
//	["a","b"]  ->  ["a    and    "b]
//	["a"]      ->  ["a"]
//
// None of which is the name of anything. So the store poll that is supposed to
// pick up a signal ALREADY waiting could never match, and the workflow suspended
// instead -- with nothing left to wake it, because the delivery that would have
// done so had already arrived. Noted as pre-existing in #974 and fixed here.

func awaitWithNames(s *execSession, names string) (timedOut bool) {
	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	r := s.DurableAwaitSignals(c, nil, names, 20000, 0, 200, 256, 200)
	// Layout per packAwaitSignalsResult: sigNameLen<<48 | payloadLen<<32 |
	// timedOut<<16 | errCode.
	return (uint64(r)>>16)&0xFFFF == 1
}

func sessionHoldingSignal(t *testing.T, name string) *execSession {
	t.Helper()
	eng := NewEngine(nil, &mockCaller{},
		WithSignalStore(&suspendProbeStore{deliveries: map[string]SignalDelivery{
			name: {Payload: `{"ok":true}`},
		}}),
		WithWorkflowID("wf-1"))
	return &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: false,
	}
}

// TestTheFreshPathFindsASignalTheGuestNamedAsJSON is the defect.
//
// The signal is in the store before the await runs. The fresh path must find it
// and return it, not suspend -- because nothing will arrive later to wake this
// workflow, so suspending here means waiting out the full timeout for a signal
// that was delivered before it started waiting.
func TestTheFreshPathFindsASignalTheGuestNamedAsJSON(t *testing.T) {
	for _, names := range []string{`["a","b"]`, `["a"]`, `a,b`, `a`} {
		t.Run(names, func(t *testing.T) {
			s := sessionHoldingSignal(t, "a")

			timedOut := awaitWithNames(s, names)

			if s.suspendErr != nil {
				t.Fatalf("await(%s) suspended with the signal already in the store.\n\n"+
					"Nothing will wake this workflow: the delivery that would have done so "+
					"has already happened, so it waits out the full timeout.", names)
			}
			if timedOut {
				t.Errorf("await(%s) reported a timeout with the signal already in the store", names)
			}
		})
	}
}

// TestBothArmsParseTheNamesTheSameWay is the general statement, and it is the
// one that keeps this fixed: the two paths read one field, so they must agree
// on every form, including forms neither is expected to see.
func TestBothArmsParseTheNamesTheSameWay(t *testing.T) {
	for _, in := range []string{
		`["a","b"]`, `["a"]`, `[]`, `a,b`, `a`, ``,
		`["a", "b"]`, // whitespace after the comma
		`["a,b"]`,    // a comma INSIDE a name -- the case the splitter gets wrong
		`not json`,   // falls back
	} {
		fresh := parseSignalNames(in)
		// The replay arm's parse, transcribed from engine/signaller.go so the
		// two are compared rather than assumed identical.
		var replay []string
		if err := json.Unmarshal([]byte(in), &replay); err != nil {
			replay = splitSignalNames(in)
		}
		if len(fresh) != len(replay) {
			t.Errorf("%q: fresh=%q replay=%q -- the two arms disagree", in, fresh, replay)
			continue
		}
		for i := range fresh {
			if fresh[i] != replay[i] {
				t.Errorf("%q: fresh=%q replay=%q -- the two arms disagree", in, fresh, replay)
				break
			}
		}
	}
}
