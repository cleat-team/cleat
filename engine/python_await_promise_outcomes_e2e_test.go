//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3425: durable-await-promise was `-> string`, so a Python component guest
// could not tell how a wait ended. Measured on a real componentized guest
// before the change (h.await_promise_ms, develop at 1e7bd43e):
//
//	resolved "hello"      result="hello"                 timed_out=False rejected=False
//	resolved ""           result=""                      timed_out=False rejected=False
//	REJECTED "boom..."    result="boom..."               timed_out=False rejected=False
//	host error (no store) result="no promise store ..."  timed_out=False rejected=False
//	genuine TIMEOUT       result=""                      timed_out=False rejected=False
//	fresh suspend         guest received ""; run ended suspended
//
// Every row but the first two is a wrong answer delivered as a value. It is now
// `result<await-outcome, call-failure>`; these tests drive each ending through a
// real component and a real engine.
//
// The tests are separate top-level functions on purpose: a t.Run loop skips as
// SUBTESTS while its parent reports PASS, which the skip ledger cannot match with
// a whole-test regex (cleat#3437).

// awaitOutcomeFixture is the examples/ file these tests compile.
const awaitOutcomeFixture = "await_promise_outcomes_workflow.py"

var awaitOutcomeInput = json.RawMessage(`{"promise_id":"p1","timeout_ms":60000}`)

type awaitOutcomeDoc struct {
	Result   string `json:"result"`
	TimedOut bool   `json:"timed_out"`
	Rejected bool   `json:"rejected"`
	Raised   string `json:"raised"`
	Message  string `json:"message"`
}

func decodeAwaitOutcome(t *testing.T, result string) awaitOutcomeDoc {
	t.Helper()
	var d awaitOutcomeDoc
	if err := json.Unmarshal([]byte(unwrapPythonResult(t, result)), &d); err != nil {
		t.Fatalf("workflow result %q is not the fixture's JSON document: %v", result, err)
	}
	return d
}

// runAwaitOutcome executes the fixture against a promise store in the given
// state and returns what the SDK reported and the operations the guest made.
func runAwaitOutcome(t *testing.T, store *mockPromiseStore) (awaitOutcomeDoc, []string) {
	t.Helper()
	caller := &mockCaller{}
	var opts []EngineOption
	if store != nil {
		opts = append(opts, WithPromiseStore(store))
	}
	wasmBytes, eng := pythonEntryEngine(t, awaitOutcomeFixture, "await_outcome", caller, opts...)
	result, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "await_outcome", awaitOutcomeInput)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("the run suspended (%s) though the promise had settled", suspended.Reason)
	}
	return decodeAwaitOutcome(t, result), operationsCalled(caller)
}

func TestPythonGuestAwaitPromiseResolvedValuesAreResolvedIncludingTheEmptyOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	got, ops := runAwaitOutcome(t, &mockPromiseStore{status: "resolved", result: "hello"})
	if got.Result != "hello" || got.TimedOut || got.Rejected || got.Raised != "" {
		t.Errorf("resolved \"hello\": got %+v, want result=hello and no flags", got)
	}
	if len(ops) != 1 {
		t.Errorf("a resolved wait must let the guest run on; operations = %v", ops)
	}

	got, _ = runAwaitOutcome(t, &mockPromiseStore{status: "resolved", result: ""})
	if got.Result != "" || got.TimedOut || got.Rejected || got.Raised != "" {
		t.Errorf("resolved \"\": got %+v, want an empty result and no flags", got)
	}
}

func TestPythonGuestAwaitPromiseRejectedIsARejectionWithItsReason(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const reason = "boom: the promise was rejected"
	got, ops := runAwaitOutcome(t, &mockPromiseStore{status: "rejected", errMsg: reason})
	if !got.Rejected || got.Result != reason || got.TimedOut || got.Raised != "" {
		t.Errorf("rejected: got %+v, want rejected=True with result=%q (measured before the change: "+
			"the reason arrived with rejected=False, as if the promise had resolved to it)", got, reason)
	}
	if len(ops) != 1 {
		t.Errorf("a rejection is an outcome the workflow handles, so the guest runs on; operations = %v", ops)
	}
}

func TestPythonGuestAwaitPromiseHostErrorIsACallFailureNotAValue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	// nil store: the engine has no promise store, so the host reports an error.
	got, ops := runAwaitOutcome(t, nil)
	if got.Raised == "" {
		t.Fatalf("a host error reached the guest as a value: %+v (measured before the change: the "+
			"error text arrived as the resolved result with no exception)", got)
	}
	if !strings.Contains(got.Message, "no promise store configured") {
		t.Errorf("raised %s with message %q, want the host's reason in it", got.Raised, got.Message)
	}
	if len(ops) != 0 {
		t.Errorf("the guest ran on after a host error and made %v", ops)
	}
}

func TestPythonGuestAwaitPromiseTimeoutIsATimeoutNotAnEmptyValue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	caller := &mockCaller{}
	store := &mockPromiseStore{status: ""}
	wasmBytes, eng := pythonEntryEngine(t, awaitOutcomeFixture, "await_outcome", caller, WithPromiseStore(store))
	ctx := context.Background()

	// First segment: the promise is pending, the wait is recorded, the run suspends.
	_, hist, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "await_outcome", awaitOutcomeInput)
	if err != nil || suspended == nil {
		t.Fatalf("first segment: suspended=%v err=%v, want a suspension at the wait", suspended != nil, err)
	}
	// Second segment: the recorded await is moved past its 60s deadline, as
	// a_suspending_await_promise_stops_the_guest_test.go does.
	moved := 0
	for i := range hist {
		if hist[i].EventType == EventTypeAwaitPromise {
			hist[i].TimestampMs -= 61_000
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("expected one await_promise record to move, found %d", moved)
	}
	result, _, suspended, _, _, err := eng.Replay(ctx, wasmBytes, "await_outcome", awaitOutcomeInput, hist)
	if err != nil || suspended != nil {
		t.Fatalf("second segment: suspended=%v err=%v, want it to finish on the timeout", suspended != nil, err)
	}
	got := decodeAwaitOutcome(t, result)
	if !got.TimedOut || got.Result != "" || got.Rejected || got.Raised != "" {
		t.Errorf("timeout: got %+v, want timed_out=True (measured before the change: timed_out=False "+
			"with result=\"\", identical to a promise resolved with an empty value)", got)
	}
}

func TestPythonGuestAwaitPromiseFreshSuspendStopsTheGuestAtTheWait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	caller := &mockCaller{}
	wasmBytes, eng := pythonEntryEngine(t, awaitOutcomeFixture, "await_outcome", caller,
		WithPromiseStore(&mockPromiseStore{status: ""}))
	_, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "await_outcome", awaitOutcomeInput)
	if err != nil {
		t.Fatalf("Execute: %v -- the run FAILED where it should have suspended", err)
	}
	if suspended == nil {
		t.Fatal("the run completed; with the promise pending it should have suspended")
	}
	if ops := operationsCalled(caller); len(ops) != 0 {
		t.Errorf("the guest ran on past the pending wait and made %v before the run suspended", ops)
	}
	if len(hist) != 1 || hist[0].EventType != EventTypeAwaitPromise {
		t.Errorf("history = %+v, want exactly the recorded await_promise", hist)
	}
}

// A component built before 0.5.0 declares durable-await-promise `-> string`.
// This host no longer serves that, and the refusal has to name the import and
// the remedy. wasmtime's own message, measured on a real pre-change component,
// was "type mismatch: expected string, found result" from inside
// `adapter durable-await-promise` -- and it arrived only AFTER the host had run
// the await and recorded its event.
//
// A pre-0.5.0 component is built here, not shipped as a 19 MB binary: a copy of
// the SDK whose WIT and Python import are reverted to the old shapes.
func TestPythonGuestBuiltBeforeAwaitOutcomeIsRefusedByNameAndBeforeTheAwaitRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	helper := newPythonWasmTestHelper(t)
	if !helper.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, helper.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + helper.missingTools())
	}

	old := &pythonWasmTestHelper{t: t, repoRoot: helper.repoRoot, sdkRoot: copyPre050SDK(t, helper.sdkRoot)}
	wasmBytes, err := os.ReadFile(old.compileWorkflow(t, awaitOutcomeFixture, "await_outcome"))
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	store := &mockPromiseStore{status: "resolved", result: "x"}
	eng := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt), WithPromiseStore(store))

	_, hist, _, _, _, err := eng.Execute(ctx, wasmBytes, "await_outcome", awaitOutcomeInput)
	if err == nil {
		t.Fatal("a pre-0.5.0 component ran to completion against a host that no longer serves its import")
	}
	for _, want := range []string{"durable-await-promise", "pre-0.5.0", "Rebuild the workflow with the 0.5.0 Python SDK"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "type mismatch") {
		t.Errorf("the refusal is wasmtime's own type-mismatch message, not ours:\n%v", err)
	}
	for _, rec := range hist {
		if rec.EventType == EventTypePromiseResolved || rec.EventType == EventTypePromiseRejected {
			t.Errorf("the host ran the await (%s recorded) before refusing the component", rec.EventType)
		}
	}
}

// copyPre050SDK copies python-sdk into a temp dir and reverts durable-await-promise
// to its pre-0.5.0 shapes: the WIT signature back to `-> string`, and the three
// AwaitOutcome imports (which would not exist in that WIT) to placeholders.
// Each replacement is asserted to apply, so a refactor that moves the text fails
// here instead of silently building a NEW-shape component and testing nothing.
func copyPre050SDK(t *testing.T, sdkRoot string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "python-sdk")
	for _, dir := range []string{"cleat_sdk", "wit", "scripts", "examples"} {
		copyTree(t, filepath.Join(sdkRoot, dir), filepath.Join(dst, dir))
	}

	witPath := filepath.Join(dst, "wit", "cleat.wit")
	wit, err := os.ReadFile(witPath)
	if err != nil {
		t.Fatal(err)
	}
	const newSig = "durable-await-promise: func(\n        id: string,\n        timeout-ms: u64,\n    ) -> result<await-outcome, call-failure>;"
	const oldSig = "durable-await-promise: func(\n        id: string,\n        timeout-ms: u64,\n    ) -> string;"
	if strings.Count(string(wit), newSig) != 1 {
		t.Fatal("cleat.wit no longer contains the new durable-await-promise signature in the expected form")
	}
	writeFile(t, witPath, strings.Replace(string(wit), newSig, oldSig, 1))

	hcPath := filepath.Join(dst, "cleat_sdk", "host_calls.py")
	hc, err := os.ReadFile(hcPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(hc)
	for _, name := range []string{"Rejected", "Resolved", "TimedOut"} {
		imp := "    from wit_world.imports.durable_promises import (\n        AwaitOutcome_" + name + " as _AwaitOutcome" + name + ",\n    )\n"
		if strings.Count(src, imp) != 1 {
			t.Fatalf("host_calls.py no longer imports AwaitOutcome_%s in the expected form", name)
		}
		src = strings.Replace(src, imp, "    _AwaitOutcome"+name+" = type(\"_AwaitOutcome"+name+"\", (), {})\n", 1)
	}
	writeFile(t, hcPath, src)
	return dst
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if info.IsDir() {
			if info.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, info.Mode())
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
