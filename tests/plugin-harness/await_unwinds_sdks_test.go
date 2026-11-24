package pluginharness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

// assertAwaitCallsHonourTheSuspendSentinel is the Java half of what
// TestGoAwaitCallsUnwindThroughTheSuspendSentinel does for Go
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
// side_effect after the await returns (see HostCallFixture.afterAwait). With a correct wrapper the history is
// [await]: the guest STOPS at the await, so the side_effect is never recorded.
//
// This said [await, side_effect] until cleat#4285. That was the other reading of "correct": a wrapper that returned an error value for the
// suspend word and let the guest carry on recorded one side_effect, and the test took that for the right answer, so it pinned the behaviour
// #4285 calls a defect (the call after a pending await is recorded and runs again on the resume). All three SDK calls now throw SuspendSignal, as
// sleep does, so the AwaitChild and AwaitAnyChild rows are no longer controls of a different behaviour: they are three sites of one rule.
//
//   - Java, before cleat#3430: [await_all_children, side_effect, side_effect]. The
//     wrapper handed the workflow an Ok whose value was the whole 64 KiB
//     output buffer, and the fixture recorded that it received one.
//
// Each await is checked on its own, so a site that regressed fails its own row.
func assertAwaitCallsHonourTheSuspendSentinel(t *testing.T, wasmBytes []byte) {
	t.Helper()

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	cases := []struct {
		call      string
		awaitType engine.EventType
		reasonHas string
	}{
		{"AwaitChild", engine.EventTypeAwaitChild, "await_child("},
		{"AwaitAnyChild", engine.EventTypeAwaitAnyChild, "await_any_child("},
		{"AwaitAllChildren", engine.EventTypeAwaitAllChildren, "await_all_children("},
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
			result, _, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "exercise_host_call", input)
			if err != nil {
				t.Fatalf("engine error: %v", err)
			}
			if suspended == nil {
				t.Fatalf("the run did not suspend; the child cannot have completed, so this probe measured nothing")
			}
			// What the history below cannot say, this can, in part: the suspension is the host's own record of the await, so its reason names the
			// call, and a guest that reported an error result did not stop cleanly. (A guest that died on some OTHER exception is not separable
			// from one that threw SuspendSignal here, because the engine reports a suspension either way; the Java SDK's own unit tests are what
			// pin the wrapper's catch.)
			if !strings.Contains(suspended.Reason, c.reasonHas) {
				t.Errorf("suspension reason = %q, want it to name %q", suspended.Reason, c.reasonHas)
			}
			if strings.Contains(result, "error") {
				t.Errorf("the guest reported an error result %q: it did not stop cleanly at the await", result)
			}
			ran++

			var types []engine.EventType
			for _, e := range suspended.History {
				types = append(types, e.EventType)
			}
			want := []engine.EventType{c.awaitType}
			if len(types) != len(want) || types[0] != want[0] {
				t.Errorf("history = %v, want %v. A side_effect after the await means the guest was told to suspend and ran on "+
					"(cleat#4285: its next call is recorded after the pending await and runs again on the resume); two "+
					"mean it was handed an Ok built from the sentinel (cleat#3430). The engine reports 'suspended' in "+
					"every case, which is why the table row cannot catch this", types, want)
			}
		})
	}
	if ran != len(cases) {
		t.Errorf("ran %d of %d await calls; a green must not rest on the ones that were reached", ran, len(cases))
	}
}

func TestJavaAwaitCallsHonourTheSuspendSentinel(t *testing.T) {
	assertAwaitCallsHonourTheSuspendSentinel(t, buildJavaHostCallWasm(t))
}
