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
// await (recordUnwind) and records a side_effect when it caught the panic. A
// guest that unwound leaves that event AFTER the await record; a guest that
// trapped cannot run recover() and leaves nothing. AwaitChild and
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
			_, _, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "exercise_host_call", input)
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
			want := []engine.EventType{c.awaitType, engine.EventTypeSideEffect}
			if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] {
				t.Errorf("history = %v, want %v. The side_effect after the await is the guest's own recover() "+
					"recording that the adapter panicked with cleat.ErrSuspend; its absence means the guest did not "+
					"unwind (it trapped, or ran on). The engine still reports 'suspended' in that case, which is why "+
					"the table row cannot catch this (cleat#3385)", types, want)
			}
		})
	}
	if ran != len(cases) {
		t.Errorf("ran %d of %d await calls; a green must not rest on the ones that were reached", ran, len(cases))
	}
}
