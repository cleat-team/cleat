package pluginharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
)

// javaHostCallOutcomes is the recorded-outcome table for the Java SDK.
//
// Written from a recording run (CLEAT_HOSTCALL_RECORD=1), not from a guess.
// Every `why` says what the row would CATCH, not what it asserts: a `why` that
// restates the assertion is how a row goes green through the defect it points
// at, which is what the AwaitAllChildren rows did until #758.
//
// No row asserts a COUNT. "1 child result(s)" was green on two SDKs over a
// child result whose content was an error, for as long as that row existed.
var javaHostCallOutcomes = map[string]expectedOutcome{
	// ---- calls that suspend ----
	"AwaitChild": {
		status: statusSuspended, detailContains: "await_child(00000000-0000-0000-0000-000000000001)",
		why: "catches a Java binding that returned a value instead of propagating SuspendSignal -- the exception must cross the entry wrapper, and a fixture-level catch(RuntimeException) would swallow it and report a fabricated ok. The reason carries the run ID the guest passed, so it also catches an argument that never left the guest",
	},
	"AwaitAnyChild": {
		status: statusSuspended, detailContains: `await_any_child(["00000000-0000-0000-0000-000000000001"])`,
		why: "catches a String[] that did not marshal: the reason echoes the array as JSON, so an empty or malformed array shows here rather than passing as a suspend",
	},
	"AwaitAllChildren": {
		status: statusSuspended, detailContains: "await_all_children(00000000-0000-0000-0000-000000000001)",
		why: "catches a regression of §3.309 from the Java side. This suspends only because #754 made an incomplete child suspend instead of recording \"child not completed\" as its permanent result; before that it returned ok here, as it did on Go, Rust and AS",
	},
	"AwaitPromise": {
		status: statusSuspended, detailContains: "await_promise(00000000-0000-0000-0000-000000000002)",
		why: "catches awaitPromiseMs passing its timeout in the wrong unit -- the seconds variant would still suspend, so only the ID in the reason distinguishes the two bindings here; the unit itself is asserted by DurableAwaitSignals below",
	},
	"DurableAwaitSignals": {
		status: statusSuspended, detailContains: `await_signals(["harness-signal"], 10ms)`,
		why: "catches a millisecond/second unit error, which is invisible everywhere else: the host echoes the timeout it received, so awaitSignals(…, 10) instead of awaitSignalsMs(…, 10) reads as 10s here and nothing else changes",
	},

	// ---- calls that succeed ----
	"AcquireLock": {
		status: statusOK, detailContains: "first=true second=true",
		why: "catches a guest that fabricates the acquired flag: the SUCCESS path decodes a host-computed bit, (result>>8)&1, not a buffer, so a binding returning constant true compiles and is silently wrong about holding a lock. LIMITATION, stated so the row is not read as stronger: the in-memory lock is re-entrant for the same holder, so both are true and this row cannot yet tell a decoded bit from a hardcoded one -- closing that needs a second holder, which one invocation cannot provide",
	},
	"ChildWorkflow": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "catches a run ID the guest invented rather than read back: the suffix is host-generated, so a fabricated ID fails the shape. The value is not asserted because it varies per run",
	},
	"ChildWorkflowWithOptions": {
		status: statusOK, detailRegex: `^child-child-workflow-[0-9a-f]{8}$`,
		why: "catches the arity defect this fixture found on its first run: the Java import declared nine parameters against the host's ten (no priority), which does not fail this call -- it stops the whole MODULE instantiating, so every row above and below dies with it. That is why this row is worth having even though it looks identical to ChildWorkflow: the two differ only in which import must link",
	},
	"CreatePromise": {
		status: statusOK, detailRegex: uuidShape,
		why: "catches a guest that discarded the host's output buffer: the host allocates the promise ID, so a binding that returned empty or a locally generated value fails the shape",
	},
	"DurableCall": {
		status: statusOK, detailContains: "{}",
		why: "catches a guest that does not DECODE the response, which is exactly where §3.200 failed -- the mock caller answers every service with {}, so the assertion is that something came back through the result buffer at all",
	},
	"DurableCallWithHeartbeat": {
		status: statusOK, detailContains: "{}",
		why: "catches the heartbeat variant diverging from the plain call: same response, different host function, so a binding that packed its extra i64 wrongly would fail to link or return an error rather than {}",
	},
	"DurableCallWithRetry": {
		status: statusOK, detailContains: "{}",
		why: "catches a RetryPolicy that did not marshal: the Java binding is generic with no string-in/string-out form and serialises nonRetryableErrors to JSON, so a policy that failed to encode surfaces as an error here. maxAttempts is 1 so the row does not depend on backoff timing",
	},
	"DurableDefer": {
		status: statusOK, detailContains: "defer-0",
		why: "catches a guest that fabricated a defer ID: the host returns a sequential ID, not a UUID, so a locally generated value would not read defer-0",
	},
	"DurableDeferFunc": {
		status: statusOK, detailContains: "defer-0",
		why: "catches deferFunc not reaching the host at all. It returns the same sequential ID as DurableDefer because each invocation is a fresh workflow, so the counter restarts -- the row proves the Runnable form registers, NOT that the body ran, which happens after this invocation ends",
	},
	"PluginCall": {
		status: statusOK, detailContains: `ok={"providers":{}} err=blobstore: no tenant context`,
		why: "catches §3.200 from the Java side, and it is the row WS-1 asked this fixture to exercise deliberately: llm.list_models is the one plugin call that works with no tenant, so the success path alone would prove nothing -- the defect lived entirely on the ERROR path, where a guest decoded the host's message length and threw the message away. blobstore.put supplies that path, and the host's own text is asserted, so a guest that discards it changes this row and nothing else. Identical to the Go row's detail, which is the point: plugin_call is the least parity-protected host call in the repo",
	},
	"PollCancellation": {
		status: statusOK, detailContains: "false",
		why: "catches a boolean that did not cross the boundary: nothing cancelled this workflow, so false is the answer, and a binding returning a default would be indistinguishable only if it defaulted to false -- which is why the cancelled case belongs in a later wave rather than being claimed here",
	},
	"PollChild": {
		status: statusOK, detailContains: "running",
		why: "catches the status field being dropped: a child that was never started reads as running rather than as an error, which is the in-memory store's measured answer",
	},
	"RunID": {
		status: statusOK, detailRegex: uuidShape,
		why: "catches an empty or fabricated run ID. LIMITATION: RunID and WorkflowID are the SAME value in this environment, so neither row can catch a binding that returns one for the other -- closing that would mean diverging the env from what it models, which is a worse trade",
	},
	"WorkflowID": {
		status: statusOK, detailRegex: uuidShape,
		why: "same shape and the same limitation as RunID, asserted separately so that breaking one binding cannot leave the other's row green",
	},
	"SideEffect": {
		status: statusOK, detailContains: "side-effect-value",
		why: "catches the value not round-tripping through the host; §3.204 was this call failing to compile at all in some SDKs. LIMITATION: Java's binding takes an already-computed String rather than a closure, so this row does NOT prove the host suppressed a recomputation on replay, which Go's closure form can",
	},

	// ---- calls that fail for a named environmental reason ----
	"PluginCallStreaming": {
		status: statusError, detailContains: "plugin_call_streaming: no plugin stream registry configured",
		why: "catches the host's message being discarded on the streaming path specifically -- the twin of PluginCall, and the second half of what #730 fixed. The in-memory env wires no stream registry, the same reason llm.chat_stream is a known failure in the plugin harness",
	},
	"PollSignal": {
		status: statusError, detailContains: "signal not found: harness-signal",
		why: "catches a divergence worth naming rather than smoothing over: Go records this call as OK with present=false, and Java reports an ERROR, because CleatResult<String> has no present channel and absence has nowhere to go but the error case. So a Java workflow polling a signal cannot distinguish \"not yet\" from \"broken\". Recorded as measured and NOT endorsed -- if the Java binding grows a present flag this row must change, which is the point of asserting the text",
	},

	// ---- calls the Java SDK does not bind ----
	"ScheduleCron": {
		status: statusUnsupported, detailContains: "no cleat_schedule_cron import in the Java SDK",
		why: "catches the gap closing silently. grep -rn cron over crates/cleat-java/src/main/java/cleat/ returns nothing, while the host exports cleat_schedule_cron and the AssemblyScript SDK binds it -- so this is a guest-side gap, not a host limitation. The day Java gains the binding this row stops matching and somebody has to decide what the right answer is",
	},
	"ListCrons": {
		status: statusUnsupported, detailContains: "no cleat_list_crons import in the Java SDK",
		why: "same gap as ScheduleCron, asserted separately so that adding one binding and not the other cannot leave a green row behind",
	},
}

func buildJavaHostCallWasm(t *testing.T) []byte {
	t.Helper()

	workflowDir := hostCallFixtureDir(t, "hostcallsjava")

	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("Java not available — install JDK 11+ from https://adoptium.net")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t), "build", "--target", "java", "-o", tmpDir, workflowDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (java) failed:\n%s\n%v", string(out), err)
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
			t.Logf("built Java host-call fixture (%d bytes)", len(b))
			return b
		}
	}
	t.Fatalf("cleat build (java) reported success but produced no .wasm in %s", tmpDir)
	return nil
}

func TestHostCallsJava(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildJavaHostCallWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	invoked := 0
	for _, call := range wave1Calls {
		t.Run(call, func(t *testing.T) {
			got := executeOneCall(t, wenv.H(), wasmBytes, call, resultJSONWrapped)
			invoked++
			if recordMode() {
				recordOutcome(got)
			}
			want, known := javaHostCallOutcomes[call]
			assertOutcome(t, "java", got, want, known)
		})
	}
	if invoked != len(wave1Calls) {
		t.Errorf("invoked %d of %d wave-1 calls. A run that exercised fewer "+
			"must not pass on the strength of the ones it did reach.",
			invoked, len(wave1Calls))
	}
}

// TestJavaHostCallTableCoversEveryWave1Call guards the table against drift.
func TestJavaHostCallTableCoversEveryWave1Call(t *testing.T) {
	for _, call := range wave1Calls {
		if _, ok := javaHostCallOutcomes[call]; !ok {
			t.Errorf("wave-1 call %s has no row in javaHostCallOutcomes", call)
		}
	}
	for call := range javaHostCallOutcomes {
		found := false
		for _, c := range wave1Calls {
			if c == call {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("javaHostCallOutcomes has a row for %s, which is not a wave-1 call", call)
		}
	}
}
