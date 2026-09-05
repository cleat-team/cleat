package pluginharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// rustHostCallOutcomes is the expected-outcome table for Rust.
//
// MEASURED, not predicted, exactly as the Go table was: every row was recorded
// with CLEAT_HOSTCALL_RECORD=1 and then given a reason.
var rustHostCallOutcomes = map[string]expectedOutcome{
	// ---- calls that suspend ----
	//
	// All four match Go exactly, including the rendered arguments. That is the
	// point of mirroring the fixture argument for argument: the suspend reason
	// is the only view the harness gets of what the guest actually passed, so
	// an identical reason is evidence the two SDKs encoded the same values the
	// same way.
	"AwaitChild": {
		status: statusSuspended, detailContains: "await_child(00000000-0000-0000-0000-000000000001)",
		why: "identical to Go's row; the run ID the guest passed survives into the suspend reason",
	},
	"AwaitAnyChild": {
		status: statusSuspended, detailContains: `await_any_child(["00000000-0000-0000-0000-000000000001"])`,
		why: "identical to Go's row, JSON array and all -- so &[&str] on the Rust side encodes the same wire shape as []string on the Go side",
	},
	"AwaitPromise": {
		status: statusSuspended, detailContains: "await_promise(00000000-0000-0000-0000-000000000002)",
		why: "identical to Go's row; a promise with no resolver suspends rather than timing out",
	},
	"DurableAwaitSignals": {
		status: statusSuspended, detailContains: `await_signals(["harness-signal"], 10ms)`,
		why: "identical to Go's row INCLUDING the 10ms. Rust passes a Duration and Go passes an int64 of milliseconds, and both arrive as the same duration -- which is the unit-crossing §3.202 was about",
	},

	// ---- locks ----
	"AcquireLock": {
		status: statusOK, detailContains: "first=true second=true",
		why: "matches Go. Same LIMITATION as the Go row and for the same reason: the in-memory lock is re-entrant for the same holder, so both acquisitions return true and neither row can distinguish a decoded bit from a hardcoded one. Rust passes a Duration where Go passes an int64 of milliseconds; that difference is invisible here because the value is never read back",
	},

	// ---- calls that succeed ----
	"AwaitAllChildren": {
		status: statusOK, detailContains: "1 child result(s)",
		why: "matches Go's count. Go's binding returns a typed slice and Rust's returns the raw JSON array, so the fixture counts the decoded array -- the agreement is about the host's answer, not about the binding shape",
	},
	"ChildWorkflow": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "same shape as Go; the run ID is generated, so the shape is asserted rather than the value",
	},
	"ChildWorkflowWithOptions": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "same as ChildWorkflow, and carrying the same caveat as Go's row: the in-memory store ignores Version, so this does NOT prove the options crossed the boundary",
	},
	"CreatePromise": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host allocates the promise ID and the guest reads it back out of the result buffer; a guest that discarded it would return empty and fail the shape",
	},
	"DurableCall": {
		status: statusOK, detailContains: "{}",
		why: "the mock caller answers every service with {}; the row is about the guest DECODING a response, which is where §3.200 failed",
	},
	"DurableCallWithHeartbeat": {
		status: statusOK, detailContains: "{}",
		why: "matches Go, but proves LESS: Go's binding takes a progress callback and Rust's cleat_call_heartbeat takes none, so nothing here exercises progress delivery in either language",
	},
	"DurableCallWithRetry": {
		status: statusOK, detailContains: "{}",
		why: "matches Go, and proves something slightly different: the Rust binding is generic over serde types with no string-in/string-out form, so the request and response round-trip through serde_json::Value. The first attempt succeeds in both languages, so neither exercises the retry policy",
	},
	"DurableDefer": {
		status: statusOK, detailContains: "defer-0",
		why: "matches Go: registering a defer returns a sequential host-assigned ID, not a UUID. A guest fabricating an ID locally would not produce this",
	},
	"DurableDeferFunc": {
		status: statusOK, detailContains: "defer-0",
		why: "matches Go, and for the same reason it is defer-0 and not defer-1: each call runs in its own invocation, so the two do NOT share a counter",
	},
	"PluginCall": {
		status: statusOK, detailContains: `ok={"providers":{}} err=blobstore: no tenant context`,
		why: "both paths in one row, matching Go byte for byte. The error half is the load-bearing one: §3.200 was a guest that decoded the host's error length and discarded the message, so asserting the host's own text is what makes reverting that fix redden this row",
	},
	"PollCancellation": {
		status: statusOK, detailContains: "cancelled=false",
		why: "matches Go; nothing cancels the workflow, and the empty reason is the second return a guest must not drop",
	},

	// ---- calls that agree with Go on the ANSWER and differ on the SHAPE ----
	"PollChild": {
		status: statusOK, detailContains: `{"status":"running"}`,
		why: "Go's row says `running` and this one says {\"status\":\"running\"}, because the bindings differ: Go's PollChild returns (status, result) and Rust's returns one raw JSON string. Same host answer, different amount of decoding done by the SDK -- recorded as a difference rather than normalised away, because normalising it would hide which side does the parsing",
	},
	"PollSignal": {
		status: statusOK, detailContains: "present=false",
		why: "matches Go; present=false with an empty payload is the pair a guest must decode, and reading only the payload would look identical",
	},
	"SideEffect": {
		status: statusOK, detailContains: "side-effect-value",
		why: "the detail matches Go and the row proves LESS. Go's SideEffect takes a closure, so its row shows the closure ran and its value round-tripped; Rust's takes the already-computed value, so this row shows only the round trip. The identical detail makes that easy to miss",
	},
	"WorkflowID": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host's workflow ID reaches the guest as a string",
	},
	"RunID": {
		status: statusOK, detailRegex: uuidShape,
		why: "same caveat as Go's row, and it reproduced here: in this environment RunID and WorkflowID are the SAME value, so neither row can catch a guest that returns one for the other",
	},

	// ---- calls that fail for a named environmental reason ----
	"PluginCallStreaming": {
		status: statusError, detailContains: "plugin_call_streaming: no plugin stream registry configured",
		why: "matches Go's message exactly. It proves less than Go's row about the BINDING, though: Go's returns a channel and its fixture counts events, Rust's returns a buffered response, so no Rust row exercises streaming delivery",
	},

	// ---- calls this SDK cannot make at all ----
	//
	// The finding of C2, and the reason `unsupported` is a status rather than
	// an error. Go reports both of these as an ordinary host refusal ("no
	// workflow store configured") because Go can make the call; Rust cannot
	// make it at all. Re-derive:
	//
	//     grep -c cron crates/cleat-sdk/src/host_calls.rs                     # 0
	//     grep -c cleat_schedule_cron packages/cleat-as/assembly/host-calls.ts # non-zero
	"ScheduleCron": {
		status: statusUnsupported, detailContains: "no cleat_schedule_cron import",
		why: "the Rust SDK declares no cron imports -- zero occurrences of the string `cron` in host_calls.rs. The host exports cleat_schedule_cron and the AssemblyScript SDK binds it, so this is a guest-side gap and not a host limitation. Go's row for this call is an ERROR from the host; the difference between the two rows is the whole point",
	},
	"ListCrons": {
		status: statusUnsupported, detailContains: "no cleat_list_crons import",
		why: "same gap as ScheduleCron, asserted separately so that adding one binding and not the other cannot leave a green row behind",
	},
}

func buildRustHostCallWasm(t *testing.T) []byte {
	t.Helper()

	workflowDir := hostCallFixtureDir(t, "hostcallsrust")

	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("Rust toolchain not available — install from https://rustup.rs")
	}
	// wasm32-unknown-unknown, not wasip1: that is what `cleat build --target
	// rust` compiles for (cmd/cleat/build_rust.go). Checking the wrong target
	// is a mistake this package has already made once -- see the comment on
	// buildRustWorkflowWasm.
	checkOut, _ := exec.Command("rustup", "target", "list", "--installed").Output()
	if !strings.Contains(string(checkOut), "wasm32-unknown-unknown") {
		t.Skip("wasm32-unknown-unknown target not installed — run: rustup target add wasm32-unknown-unknown")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t), "build", "--target", "rust", "-o", tmpDir, workflowDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (rust) failed:\n%s\n%v", string(out), err)
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
			t.Logf("built Rust host-call fixture (%d bytes)", len(b))
			return b
		}
	}
	t.Fatalf("no .wasm file in cleat build output: %s", tmpDir)
	return nil
}

// TestHostCallsRust runs each wave-1 host call from a Rust guest.
func TestHostCallsRust(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildRustHostCallWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	invoked := 0
	for _, call := range wave1Calls {
		t.Run(call, func(t *testing.T) {
			got := executeOneCall(t, wenv.H(), wasmBytes, call)
			invoked++
			if recordMode() {
				recordOutcome(got)
			}
			want, known := rustHostCallOutcomes[call]
			assertOutcome(t, "rust", got, want, known)
		})
	}
	if invoked != len(wave1Calls) {
		t.Errorf("invoked %d of %d wave-1 calls. A run that exercised fewer "+
			"must not pass on the strength of the ones it did reach.",
			invoked, len(wave1Calls))
	}
}

// TestRustHostCallTableCoversEveryWave1Call guards the table against drift,
// in both directions, as the Go one does.
func TestRustHostCallTableCoversEveryWave1Call(t *testing.T) {
	for _, call := range wave1Calls {
		if _, ok := rustHostCallOutcomes[call]; !ok {
			t.Errorf("wave-1 call %s has no row in rustHostCallOutcomes", call)
		}
	}
	for call := range rustHostCallOutcomes {
		found := false
		for _, c := range wave1Calls {
			if c == call {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("rustHostCallOutcomes has a row for %s, which is not a wave-1 call.", call)
		}
	}
}
