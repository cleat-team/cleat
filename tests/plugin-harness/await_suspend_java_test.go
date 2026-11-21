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

// cleat#4285, Java: a guest that awaits a child that is still running must STOP there.
//
// The host answers a pending child await with the suspend word (1<<62, packAwaitChildResultSuspend). The Java SDK used to turn that word into
// CleatResult.err("... workflow must suspend"), an ordinary value, so a guest that handled the value and carried on made its next call, which the
// host did not refuse and recorded AFTER the pending await; on the resume that call ran again. sleep throws SuspendSignal and the three child awaits
// now do the same.
//
// WHY THE TABLE ROW CANNOT SEE THIS. hostcall_java_test.go's AwaitChild row says it "catches a Java binding that returned a value instead of
// propagating SuspendSignal". It cannot: its fixture does `return result(call, h.awaitChild(...))`, so nothing runs after the await, and the engine
// records the suspension host-side, so the row reads `suspended` whether the SDK threw or returned an error value.
//
// WHAT THIS ADDS TO TestJavaAwaitCallsHonourTheSuspendSentinel (await_unwinds_sdks_test.go), which IS guest-level: that test records a side_effect
// after the await and counts event types. It used to want [await, side_effect], which pinned the behaviour this change removes, and I did not see
// it because I ran a list of Java tests and not the package. It now wants [await]. This test differs in what it counts and in having a control:
// the fixture entry await_then_call drops the await's result on purpose and makes a DurableCall, counted at the recording caller (what the service
// received, not only what the history recorded) and in the history, with a no-await control that must make the call.
//
// WHAT IT COVERS AND WHAT IT DOES NOT. The call used is DurableCall (service harness-service, operation after_suspend), which is not yet on the
// replay step dispatcher, so the host's own guard (step 1 of #4285: refuse fresh work after a suspend) does not cover it today, and this
// assertion is what does. When that guard reaches DurableCall the HOST will refuse the call for a guest that continues, and this test will pass for
// an SDK that returns a value as well: it then no longer tells the SDK's throw from the host's refusal. At that point the assertion has to move to
// something only the guest can show (that the statement after the await was never reached), not be taken as still covering the SDK.
func TestAJavaGuestStopsAtAChildAwaitThatSuspendsInsteadOfReachingItsNextCall(t *testing.T) {
	wasmBytes := buildJavaHostCallWasm(t)

	const child = "00000000-0000-0000-0000-000000000001"
	rows := []struct {
		await       string
		wantReason  string
		wantReached bool // the control: no await, so the call is made
	}{
		{await: "AwaitChild", wantReason: "await_child(" + child + ")"},
		{await: "AwaitAllChildren", wantReason: "await_all_children(" + child + ")"},
		{await: "AwaitAnyChild", wantReason: `await_any_child(["` + child + `"])`},
		// CONTROL: the same entry with no await in front of the call makes it, so a zero count in the rows above is the guest stopping and not a
		// harness that cannot see the call.
		{await: "None", wantReached: true},
	}
	for _, r := range rows {
		r := r
		t.Run(r.await, func(t *testing.T) {
			env := NewTestPluginEnvInMemory(t)
			defer env.Close()
			wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
			defer wenv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			input, _ := json.Marshal(map[string]string{"call": r.await})
			_, history, suspended, _, _, err := wenv.H().Execute(ctx, wasmBytes, "await_then_call", input)
			if err != nil {
				t.Fatalf("engine: %v", err)
			}

			if r.wantReached {
				if suspended != nil {
					t.Fatalf("PRECONDITION FAILED: the control suspended (%q), so it did not make its call and proves nothing about the rows above", suspended.Reason)
				}
			} else {
				// The guest has to have been put in the state under test: the host answered the suspend word. A run that did not suspend here
				// never saw a pending child, and a zero count below would be a pass for the wrong reason.
				if suspended == nil {
					t.Fatalf("the run did not suspend at %s: the host never answered the suspend word, so the guest was not in the state under test", r.await)
				}
				if !strings.Contains(suspended.Reason, r.wantReason) {
					t.Fatalf("suspended with reason %q, want one containing %q", suspended.Reason, r.wantReason)
				}
			}

			// Two readings of "the call was made", which must agree: what the caller received, and what the history recorded.
			atCaller := 0
			for _, c := range wenv.Caller().Calls {
				if c.Service == "harness-service" && c.Op == "after_suspend" {
					atCaller++
				}
			}
			inHistory := 0
			for _, e := range history {
				if e.EventType == engine.EventTypeCall && e.Service == "harness-service" && e.Op == "after_suspend" {
					inHistory++
				}
			}
			want := 0
			if r.wantReached {
				want = 1
			}
			if atCaller != want || inHistory != want {
				t.Errorf("after %s the call after_suspend was made %d time(s) at the caller and recorded %d time(s), want %d and %d each.\n"+
					"A non-zero count in a suspended run is the guest carrying on after the suspend word: the SDK returned a value where it must throw SuspendSignal, "+
					"and on the resume that call runs again (cleat#4285).", r.await, atCaller, inHistory, want, want)
			}
		})
	}
}
