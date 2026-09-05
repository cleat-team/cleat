package pluginharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// goHostCallOutcomes is the expected-outcome table for Go, and Go is the
// reference implementation the other four languages are compared against.
//
// MEASURED, not predicted. Every row was recorded with
// CLEAT_HOSTCALL_RECORD=1 and then given a reason. A row whose `why` is a
// restatement of its `status` is not a reason and will not survive review.
const uuidShape = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

var goHostCallOutcomes = map[string]expectedOutcome{
	// ---- calls that suspend ----
	//
	// Suspension is the correct outcome for these four, and the reason each
	// one names its arguments is that the suspend reason is the only place the
	// harness can see what the guest actually passed. AwaitPromise suspending
	// on a promise that does not exist is not a bug: the engine cannot know it
	// will never resolve.
	"AwaitChild": {
		status: statusSuspended, detailContains: "await_child(00000000-0000-0000-0000-000000000001)",
		why: "awaiting an unresolved child suspends; the reason carries the run ID the guest passed, which is the only view of the guest's argument",
	},
	"AwaitAllChildren": {
		status: statusSuspended, detailContains: "await_all_children(00000000-0000-0000-0000-000000000001)",
		why: "an incomplete child suspends, as AwaitChild and AwaitAnyChild do on the same run ID. " +
			"This row asserted statusOK until 2026-09-05, with the raw result `[{\"run_id\":\"...\",\"error\":\"child not completed\"}]` -- and that was the defect, not the contract. " +
			"It is the row that found §3.309: WS-1 flagged the disagreement with AwaitChild, WS-2 traced it off the no-backend path onto the ordinary one, and #754 fixed both halves " +
			"(the fresh path suspends, and replayAwaitAllChildren gained the \"no cached result, re-check\" fall-through it lacked -- without which suspending alone would have replayed as an EMPTY result). " +
			"Worth keeping the history: the row asserted \"1 child result(s)\" before that, and went green through the very defect it was flagging, because a count over a result hides which result it was",
	},
	"AwaitAnyChild": {
		status: statusSuspended, detailContains: `await_any_child(["00000000-0000-0000-0000-000000000001"])`,
		why: "same as AwaitChild, and the JSON array shows the guest encoded a list rather than a single ID",
	},
	"AwaitPromise": {
		status: statusSuspended, detailContains: "await_promise(00000000-0000-0000-0000-000000000002)",
		why: "a promise with no resolver suspends indefinitely; the engine cannot know it will never resolve, so this is correct and not a timeout",
	},
	"DurableAwaitSignals": {
		status: statusSuspended, detailContains: `await_signals(["harness-signal"], 10ms)`,
		why: "no signal is delivered, so it suspends. The 10ms is the guest's timeout arriving as a duration -- §3.202 was a stop read as a timeout on this exact call, and the rendered unit is what makes that visible",
	},

	// ---- locks ----
	"AcquireLock": {
		status: statusOK, detailContains: "first=true second=true",
		why: "wave 1 on WS-3's C1 finding: the error path reads no buffer, but the SUCCESS path decodes a host-computed bit, `acquired := (result>>8)&0x1`. " +
			"LIMITATION, stated so the row is not read as stronger than it is: both acquisitions return true because the in-memory lock is re-entrant for the same holder, " +
			"so this row still cannot distinguish a decoded bit from a hardcoded true. Closing that needs a second holder, which one workflow invocation cannot provide. " +
			"Whether re-entrant true is the intended contract is undetermined here and is not a claim this row makes",
	},

	// ---- calls that succeed with no backend ----
	"ChildWorkflow": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "starting a child returns its run ID synchronously; the suffix is generated, so the shape is asserted rather than the value",
	},
	"ChildWorkflowWithOptions": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "identical to ChildWorkflow here because the in-memory store ignores Version -- so this row does NOT prove the options crossed the boundary; that is B1's gap to close, not a claim this row makes",
	},
	"CreatePromise": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host allocates the promise ID and the guest must read it back out of the result buffer; a guest that discarded it would return empty and fail the shape",
	},
	"DurableCall": {
		status: statusOK, detailContains: "{}",
		why: "the mock caller answers every service with {}; the point of the row is that the guest DECODES a response, which is where §3.200 failed",
	},
	"DurableCallWithHeartbeat": {
		status: statusOK, detailContains: "{}",
		why: "same mock caller. The heartbeat interval and progress callback are accepted and unused here -- another thing this row does not prove",
	},
	"DurableCallWithRetry": {
		status: statusOK, detailContains: "{}",
		why: "same mock caller, and the first attempt succeeds, so the retry policy is never exercised",
	},
	"DurableDefer": {
		status: statusOK, detailContains: "defer-0",
		why: "registering a defer returns a sequential ID from the host, not a UUID; a guest that fabricated an ID locally would not produce this",
	},
	"DurableDeferFunc": {
		status: statusOK, detailContains: "defer-0",
		why: "the closure form registers the same way and gets the same ID, because each runs in its own invocation -- NOT because the two share a counter",
	},
	"PluginCall": {
		status: statusOK, detailContains: `ok={"providers":{}} err=blobstore: no tenant context`,
		why: "both paths in one row. llm.list_models is the one plugin call that works with no tenant; blobstore.put supplies the ERROR path, which is where §3.200 lived -- the guest decoded the host's error length and discarded the message. Asserting the host's own text is what makes reverting that fix redden this row and no other",
	},
	"PollCancellation": {
		status: statusOK, detailContains: "cancelled=false",
		why: "nothing cancels the workflow, and the empty reason is the second return the guest must not drop",
	},
	"PollChild": {
		status: statusOK, detailContains: "running",
		why: "a child that was never started reads as running rather than as an error -- measured, and it is the answer the in-memory store gives",
	},
	"PollSignal": {
		status: statusOK, detailContains: "present=false",
		why: "no signal is pending; present=false with an empty payload is the pair a guest must decode, and reading only the payload would look identical",
	},
	"SideEffect": {
		status: statusOK, detailContains: "side-effect-value",
		why: "the guest's closure runs and its value round-trips through the host; §3.204 was this call failing to compile at all",
	},
	"WorkflowID": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host's workflow ID reaches the guest as a string",
	},
	"RunID": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host's run ID reaches the guest. NOTE: in this environment RunID and WorkflowID are the SAME value, so neither row can catch a guest that returns one for the other. Recorded so nobody reads these two passing rows as proof they are distinct",
	},

	// ---- calls that fail for a named environmental reason ----
	"ListCrons": {
		status: statusError, detailContains: "cleat_list_crons: no workflow store configured",
		why: "the in-memory env wires no workflow store. The host's own message must survive the boundary -- a guest printing a code against a legend instead is §3.200 and #734",
	},
	"ScheduleCron": {
		status: statusError, detailContains: "cleat_schedule_cron: no workflow store configured",
		why: "same missing store; asserted separately because the two calls take different paths to the same refusal and a shared substring would hide one going wrong",
	},
	"PluginCallStreaming": {
		status: statusError, detailContains: "plugin_call_streaming: no plugin stream registry configured",
		why: "the in-memory env wires no stream registry, the same reason llm.chat_stream is a known failure in the plugin harness",
	},
}

func buildGoHostCallWasm(t *testing.T) []byte {
	t.Helper()

	workflowDir := hostCallFixtureDir(t, "hostcallsgo")
	// Fatal, not Skip. check-skips.sh case (c): the precondition is always
	// satisfiable here, because `go test` is what is running this line -- a
	// machine with no Go toolchain never reaches it. A skip would be a
	// decision not to run the test, dressed as an environmental fact.
	//
	// The -short skip that was here is gone for the same reason, and it is the
	// second time this week I have shipped one: §3.204's test had it for about
	// an hour. -short is a mode someone opts into, not a precondition, and a
	// harness that measures what runs must not learn to stop running.
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain on PATH, yet `go test` is executing this: %v", err)
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t), "build", "--target", "go", "-o", tmpDir, workflowDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (go) failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built Go host-call fixture (%d bytes)", len(b))
			return b
		}
	}
	t.Fatalf("no .wasm file in cleat build output: %s", tmpDir)
	return nil
}

// TestHostCallsGo runs each wave-1 host call from a Go guest and checks its
// outcome against the table.
//
// One subtest per call, so a change localises to a call rather than to a
// fixture. That is the acceptance test for this design, not a convenience:
// §3.200 was one guest mis-decoding one call, and a harness that fails as a
// unit would have said only "Go is broken".
func TestHostCallsGo(t *testing.T) {
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildGoHostCallWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	// Count what was actually invoked and fail short.
	//
	// WS-3's C1 point 3, and §3.401 is the case behind it: a metric that
	// silently loses samples reports itself as BETTER, because the zeros sort
	// to the front and a run that measured less looks faster. A green here
	// must mean "24 calls ran", not "whatever ran, ran cleanly" -- and a
	// subtest that never executes leaves no trace of its own absence.
	invoked := 0
	for _, call := range wave1Calls {
		t.Run(call, func(t *testing.T) {
			got := executeOneCall(t, wenv.H(), wasmBytes, call)
			invoked++
			if recordMode() {
				recordOutcome(got)
			}
			want, known := goHostCallOutcomes[call]
			assertOutcome(t, "go", got, want, known)
		})
	}
	if invoked != len(wave1Calls) {
		t.Errorf("invoked %d of %d wave-1 calls. A run that exercised fewer "+
			"must not pass on the strength of the ones it did reach.",
			invoked, len(wave1Calls))
	}
}

// TestHostCallTableCoversEveryWave1Call is the guard against the table and the
// call list drifting apart.
//
// Without it the table is self-certifying: drop a row and the call stops being
// checked, with nothing to say it ever was. This is the same defect shape as a
// -run pattern that matches nothing.
func TestHostCallTableCoversEveryWave1Call(t *testing.T) {
	for _, call := range wave1Calls {
		if _, ok := goHostCallOutcomes[call]; !ok {
			t.Errorf("wave-1 call %s has no row in goHostCallOutcomes", call)
		}
	}
	for call := range goHostCallOutcomes {
		found := false
		for _, c := range wave1Calls {
			if c == call {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("goHostCallOutcomes has a row for %s, which is not a wave-1 call. "+
				"A row for a call nobody runs is a grant covering something that is not there.", call)
		}
	}
}
