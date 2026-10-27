package pluginharness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

// assertAwaitCallsHonourTheSuspendSentinel is the AssemblyScript and Java half
// of what TestGoAwaitCallsUnwindThroughTheSuspendSentinel does for Go
// (cleat#3430, found while closing cleat#3385).
//
// The engine answers AwaitChild, AwaitAnyChild and AwaitAllChildren with a
// suspend sentinel (1<<62) while a child is still running. The SDK must act on
// it before decoding. AwaitAllChildren's wrapper did not, in either SDK, and
// the harness row for it stayed green: it asserts "suspended", and the engine
// reports a suspension whether the guest handled the sentinel, died on it, or
// went on with garbage, because a suspension deliberately wins over whatever
// else happened (engine/executor.go).
//
// What distinguishes the three is the guest itself, so each fixture records a
// side_effect after the await returns (see hostcallsas/assembly/index.ts and
// HostCallFixture.afterAwait). With a correct wrapper the history is
// [await, side_effect]:
//
//   - AssemblyScript, before the fix: [await_all_children]. The sentinel was
//     parsed as a length, the decode failed, and the guest died before it could
//     record anything.
//   - Java, before the fix: [await_all_children, side_effect, side_effect]. The
//     wrapper handed the workflow an Ok whose value was the whole 64 KiB
//     output buffer, and the fixture recorded that it received one.
//
// AwaitChild and AwaitAnyChild have always checked the sentinel, so they are
// this test's own positive controls: if one stops matching, the probe is
// broken, not the wrapper under test.
func assertAwaitCallsHonourTheSuspendSentinel(t *testing.T, wasmBytes []byte) {
	t.Helper()

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
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
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
				t.Errorf("history = %v, want %v. One side_effect after the await means the guest was told to suspend "+
					"and ran on; none means it died on the sentinel; two means it was handed an Ok built from the "+
					"sentinel. The engine reports 'suspended' in all three, which is why the table row cannot catch "+
					"this (cleat#3430)", types, want)
			}
		})
	}
	if ran != len(cases) {
		t.Errorf("ran %d of %d await calls; a green must not rest on the ones that were reached", ran, len(cases))
	}
}

func TestASAwaitCallsHonourTheSuspendSentinel(t *testing.T) {
	assertAwaitCallsHonourTheSuspendSentinel(t, buildASHostCallWasm(t))
}

func TestJavaAwaitCallsHonourTheSuspendSentinel(t *testing.T) {
	assertAwaitCallsHonourTheSuspendSentinel(t, buildJavaHostCallWasm(t))
}
