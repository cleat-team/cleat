package pluginharness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

// TestGoAwaitCallsUnwindThroughTheSuspendSentinel is cleat#3385. The engine
// returns a suspend sentinel from AwaitChild, AwaitAnyChild and
// AwaitAllChildren whenever a child is still running, and the guest adapter
// must turn it into panic(cleat.ErrSuspend). AwaitAllChildren's adapter did
// not test it: it read the sentinel (1<<62) as a result length of 1<<30 and
// parsed that much memory.
//
// The row for this call in goHostCallOutcomes could not see that, and still
// cannot: it asserts the run is "suspended", and the engine reports a
// suspension whether the guest unwound or trapped, because a suspension
// deliberately wins over the error that accompanied it (engine/executor.go).
// That row was green through the whole defect.
//
// What can see it is the guest. The fixture defers recover() around each
// await (recordUnwind) and sets a query-state key when it caught the panic. A
// guest that unwound sets it; a guest that trapped cannot run recover() and
// leaves nothing. (This marker was a recorded side_effect after the await until
// cleat#4285, when the host began refusing every call made after a suspension:
// the side_effect was refused, so the test wanted a history that can no longer
// be written. The history must now hold ONLY the await, and the key is what
// proves the unwind.) AwaitChild and
// AwaitAnyChild have always honoured the sentinel, so they are this test's own
// positive controls: if they stop leaving the marker, the probe is broken, not
// the adapter.
func TestGoAwaitCallsUnwindThroughTheSuspendSentinel(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
	wasmBytes := buildGoHostCallWasm(t)
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	cases := []struct {
		call      string
		awaitType engine.EventType
	}{
		{"AwaitChild", engine.EventTypeAwaitChild},
		{"AwaitAnyChild", engine.EventTypeAwaitAnyChild},
		{"AwaitAllChildren", engine.EventTypeAwaitAllChildren},
	}
	ran := 0
	for _, c := range cases {
		t.Run(c.call, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			input, err := json.Marshal(map[string]string{"call": c.call})
			if err != nil {
				t.Fatalf("marshalling fixture input: %v", err)
			}
			_, _, suspended, _, queryState, err := wenv.H().Execute(ctx, wasmBytes, "exercise_host_call", input)
			if err != nil {
				t.Fatalf("engine error: %v", err)
			}
			if suspended == nil {
				t.Fatalf("the run did not suspend; the child cannot have completed, so this probe measured nothing")
			}
			ran++

			var types []engine.EventType
			for _, e := range suspended.History {
				types = append(types, e.EventType)
			}
			want := []engine.EventType{c.awaitType}
			if len(types) != len(want) || types[0] != want[0] {
				t.Errorf("history = %v, want %v. Anything after the await is a call the guest made after it was told to "+
					"stop, which the host refuses (cleat#4285); a guest that ran on is not an unwound one", types, want)
			}
			if queryState["guest-recovered-the-suspend"] != "1" {
				t.Errorf("query state = %v, want the guest's recover() marker set. The marker is the guest's own recover() "+
					"recording that the adapter panicked with cleat.ErrSuspend; its absence means the guest did not "+
					"unwind (it trapped, or ran on). The engine still reports 'suspended' in that case, which is why "+
					"the table row cannot catch this (cleat#3385)", queryState)
			}
		})
	}
	if ran != len(cases) {
		t.Errorf("ran %d of %d await calls; a green must not rest on the ones that were reached", ran, len(cases))
	}
}
