package pluginharness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

// TestGoStateOpsReachTheHost asserts that a Go guest's durable-state calls
// arrive at the host, by looking for them in the EVENT HISTORY.
//
// The obvious test -- SetState then GetState, assert the value comes back --
// cannot distinguish the two implementations, and that is why this defect
// survived. Until 2026-09-05 the Go SDK's state family operated on a
// guest-local map: the round trip passed, and nothing reached the engine. So
// did `TestHostCallsImpl_StateOperations`, which is still green and still
// proves nothing about this. IMPROVEMENT-PLAN 3.214.
//
// The event history is the discriminator: the host records one
// EventTypeStateMutation per operation (engine/lifecycle.go) and a map records
// nothing. Measured across the fix: 0 events before, 8 after.
func TestGoStateOpsReachTheHost(t *testing.T) {
	// NO testing.Short() SKIP. The neighbouring TestHostCalls* have one and it
	// is baselined; copying it here was reflex, and check-skips.sh was right to
	// reject it. Building a Go WASM fixture needs only the Go toolchain, which
	// is category (c) -- always satisfiable in this repo -- so a skip would be
	// a pass wearing a skip's clothing. It costs a few seconds; the defect it
	// guards shipped for months.
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
	wasmBytes := buildGoHostCallWasm(t)
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	// Instrument check. A call known to record events must produce a non-empty
	// history, or a zero below would mean "this test cannot see events" rather
	// than "the guest did not make any".
	ctlIn, _ := json.Marshal(map[string]string{"call": "DurableCall"})
	_, ctlHist, _, _, _, err := wenv.H().Execute(context.Background(), wasmBytes, "exercise_host_call", ctlIn)
	if err != nil {
		t.Fatalf("control DurableCall: %v", err)
	}
	if len(ctlHist) == 0 {
		t.Fatalf("control DurableCall recorded no history at all: this test cannot " +
			"observe event records, so its result below would be meaningless.")
	}

	in, _ := json.Marshal(map[string]string{"call": "StateRoundTrip"})
	result, history, _, _, _, err := wenv.H().Execute(context.Background(), wasmBytes, "exercise_host_call", in)
	if err != nil {
		t.Fatalf("StateRoundTrip: %v", err)
	}

	ops := map[string]int{}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeStateMutation {
			ops[ev.StateOp]++
		}
	}
	// One per operation the fixture performs. Asserted per-op rather than as a
	// total, because a total is satisfied by the wrong six.
	want := map[string]int{"set": 1, "get": 1, "has": 2, "incr": 2, "list": 1, "del": 1}
	for op, n := range want {
		if ops[op] != n {
			t.Errorf("state op %q: recorded %d times in the event history, want %d.\n"+
				"all ops seen: %v\nfixture result: %.200s\n\n"+
				"Zero for every op means the Go SDK is operating on a guest-local map "+
				"again and the host never sees the state -- see IMPROVEMENT-PLAN 3.214.",
				op, ops[op], n, ops, result)
		}
	}
}
