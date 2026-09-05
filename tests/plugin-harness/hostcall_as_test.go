package pluginharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// asHostCallOutcomes is the expected-outcome table for AssemblyScript.
//
// MEASURED, not predicted, as the Go and Rust tables were.
var asHostCallOutcomes = map[string]expectedOutcome{
	// ---- calls that suspend ----
	//
	// All four match Go and Rust exactly, rendered arguments included -- so
	// three SDKs encode the same values the same way on the wire.
	"AwaitChild": {
		status: statusSuspended, detailContains: "await_child(00000000-0000-0000-0000-000000000001)",
		why: "identical to Go's and Rust's rows",
	},
	"AwaitAnyChild": {
		status: statusSuspended, detailContains: `await_any_child(["00000000-0000-0000-0000-000000000001"])`,
		why: "identical to Go's and Rust's rows; AssemblyScript passes the run IDs as a JSON string it built by hand and it arrives the same as Go's []string",
	},
	"AwaitPromise": {
		status: statusSuspended, detailContains: "await_promise(00000000-0000-0000-0000-000000000002)",
		why: "identical to Go's and Rust's rows",
	},
	"DurableAwaitSignals": {
		status: statusSuspended, detailContains: `await_signals(["harness-signal"], 10ms)`,
		why: "identical to Go's and Rust's rows INCLUDING the 10ms, and reaching it needed awaitSignalsMs: the plain awaitSignals takes SECONDS, so it would have rounded the harness's 10ms to 0 and asked a different question. Same for awaitPromiseMs and acquireLockMs above and below",
	},

	// ---- locks ----
	"AcquireLock": {
		status: statusOK, detailContains: "first=true second=true",
		why: "matches Go and Rust, with the same limitation: the in-memory lock is re-entrant for the same holder, so both acquisitions return true and no row in any language can tell a decoded bit from a hardcoded one",
	},

	// ---- calls that succeed ----
	"ChildWorkflow": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "same shape as Go and Rust; the suffix is generated so the shape is asserted rather than the value",
	},
	"ChildWorkflowWithOptions": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "same as ChildWorkflow, with the same caveat as the other two languages: the in-memory store ignores Version, so this does not prove the options crossed the boundary",
	},
	"CreatePromise": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host allocates the promise ID and the guest reads it back out of the result buffer",
	},
	"DurableCall": {
		status: statusOK, detailContains: "{}",
		why: "the mock caller answers every service with {}; the row is about the guest decoding a response",
	},
	"DurableCallWithHeartbeat": {
		status: statusOK, detailContains: "{}",
		why: "matches Go and Rust. Like Rust and unlike Go, the AssemblyScript binding takes no progress callback, so nothing here exercises progress delivery",
	},
	"DurableCallWithRetry": {
		status: statusOK, detailContains: "{}",
		why: "matches Go and Rust. The policy is passed as flat integers with backoffCoefficient100x in hundredths, where Go passes a struct and Rust a RetryPolicy -- three encodings, same answer. The first attempt succeeds, so no language exercises the retry",
	},
	"DurableDefer": {
		status: statusOK, detailContains: "defer-0",
		why: "matches Go and Rust: a sequential host-assigned ID, not a UUID",
	},
	"DurableDeferFunc": {
		status: statusOK, detailContains: "defer-0",
		why: "matches Go and Rust, and proves something different again: AssemblyScript has no closures that survive the boundary, so deferFunc takes a top-level function plus a payload string. Go's takes a closure and Rust's takes an FnOnce. Three shapes, one host-assigned ID",
	},
	"PluginCall": {
		status: statusOK, detailContains: `ok={"providers":{}} err=blobstore: no tenant context`,
		why: "both paths in one row, matching Go and Rust byte for byte. NOTE the contrast with ScheduleCron below: pluginCall DOES carry the host's error text, so the SDK is capable of it and the calls that discard it are not doing so out of necessity",
	},
	"PollCancellation": {
		status: statusOK, detailContains: "cancelled=false",
		why: "matches Go and Rust; the empty reason is the second return a guest must not drop",
	},
	"PollChild": {
		status: statusOK, detailContains: `{"status":"running"}`,
		why: "matches RUST, not Go. Go's binding returns (status, result) and reports `running`; AssemblyScript's and Rust's return the raw JSON. Same host answer, different amount of decoding done by the SDK",
	},
	"PollSignal": {
		status: statusOK, detailContains: "present=false",
		why: "matches Go and Rust; present=false with an empty payload is the pair a guest must decode",
	},
	"SideEffect": {
		status: statusOK, detailContains: "side-effect-value",
		why: "matches Go and Rust, and proves the least of the three. Go's takes a closure, so its row shows the closure ran; AssemblyScript's and Rust's take the already-computed value, so these rows show only the round trip. AND sideEffect returns string|null here, so a FAILING side effect would arrive with no host message at all -- see AwaitAllChildren and ListCrons",
	},
	"WorkflowID": {
		status: statusOK, detailRegex: uuidShape,
		why: "the host's workflow ID reaches the guest as a string",
	},
	"RunID": {
		status: statusOK, detailRegex: uuidShape,
		why: "same caveat as the other two languages, reproduced a third time: RunID and WorkflowID are the SAME value in this environment, so neither row can catch a guest that returns one for the other",
	},

	// ---- the row that shows what the other two languages hide ----
	"AwaitAllChildren": {
		status:         statusOK,
		detailContains: `[{"run_id":"00000000-0000-0000-0000-000000000001","error":"child not completed"}]`,
		why: "reports the host's raw JSON where Go and Rust report `1 child result(s)`, and the difference is worth keeping: the single child result carries \"error\":\"child not completed\", which the count in the other two tables hides completely. Both of those rows are green over a child result that is an error. " +
			"The fixture reports raw because it cannot honestly count -- its first version counted commas and returned 2 for this one-element array, since the element is an object with a comma inside it",
	},

	// ---- calls that fail, and how badly the message survives ----
	"PluginCallStreaming": {
		status: statusError, detailContains: "plugin_call_streaming: no plugin stream registry configured",
		why: "matches Go and Rust exactly -- the host's own message, intact. This is the row to compare ScheduleCron against",
	},

	// ScheduleCron and ListCrons are the C2 findings. Both are recorded as
	// MEASURED behaviour, not as endorsed behaviour, and IMPROVEMENT-PLAN
	// carries the analysis.
	"ScheduleCron": {
		status: statusError, detailContains: "failed: timeout (code 1)",
		why: "DEFECT, recorded rather than endorsed. The AssemblyScript SDK discards the host's message and prints its own legend instead: errorCodeName(1) is \"timeout\", which this is not. " +
			"MEASURED what is lost, by patching scheduleCron to read the output buffer and re-recording: the host actually says " +
			"`no workflow store configured: workflow <uuid> cannot schedule \"harness-workflow\"` -- which names the workflow AND the schedule, and is strictly more than Go's row asserts. " +
			"host-calls.ts does this at 25 call sites, and the legend it uses (timeout/transient/not_found/invalid_request/permission_denied) is cleat_call's callErrorCode legend applied to decodeSimpleResult's errCode from a different result layout. " +
			"That is the §3.200 / #734 defect class, and wasm/adapter_hostmessage_test.go's TestNoAdapterPrintsTheCallErrorCodeLegendForAnotherLayout exists to stop the GO side doing it. Nothing checks this side. " +
			"The row asserts the wrong text on purpose: when the SDK is fixed this row reddens and gets rewritten to assert the host's message, which is how a known defect stays visible instead of being forgotten",
	},
	"ListCrons": {
		status: statusError, detailContains: "<no host message: listCrons returns string|null and discards it>",
		why: "a second, worse shape of the same loss, and this detail is written by the FIXTURE -- the SDK provides no text at all. listCrons returns `string | null`, so a failure arrives as null with the host's message already gone. " +
			"awaitAllChildren, awaitAnyChild and sideEffect have the same signature and the same hole; only listCrons is observed failing in this environment, so it is the only one this table can pin. " +
			"Rust cannot make this call at all (statusUnsupported) and Go gets the host's message. Three SDKs, three different amounts of the truth",
	},
}

func buildASHostCallWasm(t *testing.T) []byte {
	t.Helper()

	workflowDir := hostCallFixtureDir(t, "hostcallsas")

	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available — install Node.js")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t), "build", "--target", "assemblyscript", "-o", tmpDir, workflowDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (assemblyscript) failed:\n%s\n%v", string(out), err)
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
			t.Logf("built AssemblyScript host-call fixture (%d bytes)", len(b))
			return b
		}
	}
	t.Fatalf("no .wasm file in cleat build output: %s", tmpDir)
	return nil
}

// TestHostCallsAS runs each wave-1 host call from an AssemblyScript guest.
func TestHostCallsAS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildASHostCallWasm(t)

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
			want, known := asHostCallOutcomes[call]
			assertOutcome(t, "assemblyscript", got, want, known)
		})
	}
	if invoked != len(wave1Calls) {
		t.Errorf("invoked %d of %d wave-1 calls. A run that exercised fewer "+
			"must not pass on the strength of the ones it did reach.",
			invoked, len(wave1Calls))
	}
}

// TestASHostCallTableCoversEveryWave1Call guards the table against drift.
func TestASHostCallTableCoversEveryWave1Call(t *testing.T) {
	for _, call := range wave1Calls {
		if _, ok := asHostCallOutcomes[call]; !ok {
			t.Errorf("wave-1 call %s has no row in asHostCallOutcomes", call)
		}
	}
	for call := range asHostCallOutcomes {
		found := false
		for _, c := range wave1Calls {
			if c == call {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("asHostCallOutcomes has a row for %s, which is not a wave-1 call.", call)
		}
	}
}
