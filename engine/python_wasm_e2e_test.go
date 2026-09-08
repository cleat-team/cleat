package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Python WASM end-to-end test
//
// Tests the full Python -> WASM -> Host -> WASM -> Python round trip:
//   1. Compile a Python workflow to a WASM component via componentize-py
//   2. Decompose the component to a core module via wasm-tools (if needed)
//   3. Load the module into the wazero runtime
//   4. Execute the workflow (calling a mocked external service)
//   5. Verify the event history contains the expected DurableCall events
//   6. Verify the result is correctly serialized back through the ABI
// ---------------------------------------------------------------------------

// TestPythonWasmEndToEnd validates the full Python WASM execution path.
//
// Prerequisites (skipped with clear message when not found):
//   - componentize-py >= 0.12.0   (pip install componentize-py)
//   - wasm-tools                   (cargo install wasm-tools)
//   - Python 3.10+
//   - cleat-sdk installed          (pip install -e python-sdk/)
func TestPythonWasmEndToEnd(t *testing.T) {
	// Unskipped 2026-08-05. This was the acceptance test IMPROVEMENT-PLAN 2.72
	// named for whichever component instantiation path was fixed first, and the
	// native one (component_cgo.go) is now compiled into ordinary builds rather
	// than gated behind a tag nothing set. See engine.go's WasmtimeLanguages
	// comment for what that changed and what it did not.
	//
	// It had been failing on develop invisibly, because the workflow step
	// running it piped `go test` into `tee` without `set -o pipefail` -- so
	// tee's exit status was the step's. That is fixed too; without it this
	// test could pass or fail here and CI would report the same either way.

	ctx := context.Background()

	// ---- Check prerequisites ----
	//
	// toolchainRequired (defined in rust_workflow_test.go, which documents the
	// reasoning) applies here too: e2e-cross-language.yml installs
	// componentize-py and wasm-tools via pip/cargo, declares "python" in
	// CLEAT_REQUIRE_TOOLCHAINS, and states in its own header that Python is
	// first-class ("build/execute failures block CI"), while ci.yml's engine
	// matrix entry runs this package with none of that installed. A skip here
	// is only legitimate when nobody asked for these tools.
	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python, so this job installs componentize-py/wasm-tools and treats Python as first-class: %s", requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	// ---- Step 1: Compile the Python workflow to WASM ----
	wasmPath := pythonWasm.compileWorkflow(t, "durable_call_workflow.py", "durable_call_workflow")
	t.Logf("Python WASM compiled to: %s", wasmPath)

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	// ---- Step 2: Decompose the component model binary to a core module ----
	// wasm-tools >= 1.230 removed "component decompose". The wasmtime
	// backend's native Component Model path handles component binaries, so
	// we only decompose if the tool is available (for wazero fallback).
	if pythonWasm.canDecompose() {
		coreWasmPath := pythonWasm.decomposeComponent(t, wasmPath)
		if coreWasmPath != wasmPath {
			coreBytes, err := os.ReadFile(coreWasmPath)
			if err != nil {
				t.Fatalf("read decomposed WASM: %v", err)
			}
			wasmBytes = coreBytes
			t.Logf("Using decomposed core module: %s (%d bytes)", coreWasmPath, len(coreBytes))
		}
	}

	// ---- Step 3: Create runtime and engine with wasmtime backend ----
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	var engineOpts []EngineOption
	// Register exactly the languages the worker registers. This used to be an
	// unconditional `if true` block wiring WithBackend("python", wt), forcing a
	// routing the product did not use; reading WasmtimeLanguages instead means
	// this test exercises what ships. Python is now in that list, so this test
	// moved onto wasmtime without an edit here -- which was the point of
	// writing it this way (IMPROVEMENT-PLAN 2.72).
	//
	// Not a skip if the backend is missing: a wasmtime backend that fails to
	// construct is a real failure now that every language routes to it, and
	// falling back to wazero here would quietly test a configuration nothing
	// ships and report it as a pass.
	wt, wtErr := NewWasmtimeBackend(ctx)
	if wtErr != nil {
		t.Fatalf("NewWasmtimeBackend: %v (every language in WasmtimeLanguages, "+
			"python included, routes here -- there is no fallback left to test)", wtErr)
	}
	engineOpts = append(engineOpts, WithBackends(WasmtimeLanguages, wt))
	t.Logf("wasmtime registered for %v", WasmtimeLanguages)
	engine := NewEngine(rt, caller, engineOpts...)

	// ---- Step 4: Execute the workflow ----
	input := `{"request":{"user_id":"u-test-42","message":"Hello from Python WASM","channel":"email"}}`

	// The entry point exported by the Python workflow is the WIT "run" export.
	// Python WASM components generated by componentize-py always export "run"
	// as the sole entry point, which dispatches to @cleat_entry functions.
	entryPoint := "run"

	result, history, suspended, deferrals, queryState, err := engine.Execute(ctx, wasmBytes, entryPoint, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected workflow suspension: %v", suspended.Reason)
	}
	if result == "" {
		t.Error("expected non-empty result from durable_call_workflow")
	}

	t.Logf("Execution produced %d events, result=%q", len(history), result)
	t.Logf("Deferrals: %v", deferrals)
	t.Logf("QueryState: %v", queryState)

	// ---- Step 5: Verify event history ----
	// The durable_call_workflow should produce these events:
	//   0: cleat_call (notifier.SendNotification) -- wrapped by the SDK
	if len(history) < 1 {
		t.Fatalf("expected at least 1 history event, got %d: %+v", len(history), history)
	}

	// Verify the call event.
	callFound := false
	for _, rec := range history {
		if rec.EventType == EventTypeCall {
			callFound = true
			if rec.Service != "notifier" {
				t.Errorf("expected service 'notifier', got %q", rec.Service)
			}
			if rec.Op != "SendNotification" {
				t.Errorf("expected operation 'SendNotification', got %q", rec.Op)
			}
		}
		if false /* sleep events no longer recorded */ {
			t.Errorf("unexpected sleep event: %+v", rec)
		}
	}

	if !callFound {
		t.Error("expected at least one EventTypeCall event in history")
	}

	// Verify the result is valid JSON.
	var resultData any
	if err := json.Unmarshal([]byte(result), &resultData); err != nil {
		t.Errorf("result is not valid JSON: %v (raw: %s)", err, result)
	}

	t.Log("Python WASM end-to-end test passed")
}

// ---------------------------------------------------------------------------
// TestPythonWasmAbiBoundary validates the ABI between Python WASM stubs and
// the Go host without requiring compilation — it's a static verification that
// catches regressions in the host function interface.
// ---------------------------------------------------------------------------

// TestPythonWasmAbiBoundary verifies that all host functions expected by the
// Python SDK's WASM imports are registered in the Go host runtime.
func TestPythonWasmAbiBoundary(t *testing.T) {
	// # This test compared two hardcoded lists in this same file until 2026-09-07
	//
	// `pythonExpectedImports` was a literal, and so was `registeredImportNames`,
	// whose comment read "this list must stay in sync with the Export(...) calls
	// in imports.go. When adding new host functions, add them here too." Nothing
	// read imports.go and nothing read the Python SDK. The test asked whether the
	// file agreed with itself, and the answer was always yes.
	//
	// It had rotted in both directions by the time anyone looked:
	//
	//   - Both lists still named the six durable-state calls (3.216) and the two
	//     inert signal calls (3.220), which the engine had stopped exporting.
	//     Eight names this test asserted a Python workflow needs and the engine
	//     does not have -- the exact failure it exists to catch -- reported green.
	//   - `registeredImportNames` was missing thirteen real exports, including
	//     cleat_poll_update and cleat_complete_update, added days earlier.
	//
	// Both sides are now derived. See CLAUDE.md, "a check can tell you whether it
	// is consistent with itself; it cannot tell you what it is not looking at."
	hostRegistered := map[string]bool{}
	for name := range wazeroCleatABI(t) {
		hostRegistered[name] = true
	}

	pythonImports := pythonSDKHostImports(t)

	// The direction that breaks a workflow. A Python guest importing a name the
	// host does not register fails to instantiate -- it does not degrade, it does
	// not run.
	var missing []string
	for _, name := range pythonImports {
		if !hostRegistered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Python SDK imports %d host function(s) the engine does not register:\n  %s\n\n"+
			"A guest importing an unregistered name does not fail gracefully -- the module "+
			"fails to instantiate. Either the engine dropped an export the SDK still binds, "+
			"or the SDK gained a binding ahead of the host.",
			len(missing), strings.Join(missing, "\n  "))
	}

	// The other direction is a gap, not a break: the engine offers something the
	// Python SDK cannot reach. Held as a shrink-only baseline so the gaps are
	// named rather than merely absent, and so closing one is noticed.
	var unbound []string
	for name := range hostRegistered {
		if !slices.Contains(pythonImports, name) {
			unbound = append(unbound, name)
		}
	}
	sort.Strings(unbound)

	if extra := setDiff(unbound, pythonUnboundBaseline); len(extra) > 0 {
		t.Errorf("the engine exports %d host function(s) the Python SDK does not bind, "+
			"beyond the recorded baseline:\n  %s\n\n"+
			"Adding a host call without a Python binding widens the SDK gap. Either bind it "+
			"in python-sdk/cleat_sdk/host_calls.py, or add it to pythonUnboundBaseline with "+
			"a reason.", len(extra), strings.Join(extra, "\n  "))
	}
	if closed := setDiff(pythonUnboundBaseline, unbound); len(closed) > 0 {
		t.Errorf("pythonUnboundBaseline names %d host function(s) the Python SDK now binds:\n  %s\n\n"+
			"This is good news and the baseline must shrink to match, or it stops measuring "+
			"anything. Remove them from pythonUnboundBaseline.",
			len(closed), strings.Join(closed, "\n  "))
	}

	// Also verify bit-packing conventions match.
	testBitPackingCleatCall(t)
	testBitPackingSleep(t)
	testBitPackingAwaitSignals(t)
	testBitPackingSimpleResult(t)
	testBitPackingExportResult(t)
}

func testBitPackingCleatCall(t *testing.T) {
	// Verify that the Python decode_cleat_call_result and Go
	// packDurableCallResult use the same bit layout.
	//
	// Python: bits 40-63 = responseLen, bits 8-39 = callErrorCode, bits 0-7 = errCode
	// Go:     same layout (packDurableCallResult)
	responseLen := 42
	callErrorCode := byte(2)
	errCode := byte(0)

	packed := packDurableCallResult(responseLen, callErrorCode, errCode)

	// Extract using Python's decode_cleat_call_result formula.
	r := uint64(packed)
	decodedResponseLen := (r >> 40) & 0xFFFFFF
	decodedCallErrorCode := (r >> 8) & 0xFFFFFFFF
	decodedErrCode := r & 0xFF

	if decodedResponseLen != uint64(responseLen) {
		t.Errorf("responseLen mismatch: Go packed %d, Python decodes %d", responseLen, decodedResponseLen)
	}
	if decodedCallErrorCode != uint64(callErrorCode) {
		t.Errorf("callErrorCode mismatch: Go packed %d, Python decodes %d", callErrorCode, decodedCallErrorCode)
	}
	if decodedErrCode != uint64(errCode) {
		t.Errorf("errCode mismatch: Go packed %d, Python decodes %d", errCode, decodedErrCode)
	}
}

func testBitPackingSleep(t *testing.T) {
	// Verify sleep result bit packing.
	// Python: bits 56-63 = status, bits 0-55 = durationMs
	// Go:     packSleepResult has same layout
	status := byte(1)
	durationMs := int64(5000)

	packed := packSleepResult(status, durationMs)

	r := uint64(packed)
	decodedStatus := (r >> 56) & 0xFF
	_ = r & 0x00FFFFFFFFFFFFFF // durationMs (not verified)

	if decodedStatus != uint64(status) {
		t.Errorf("sleep status mismatch: Go packed %d, Python decodes %d", status, decodedStatus)
	}
}

func testBitPackingAwaitSignals(t *testing.T) {
	// Verify await_signals result bit packing.
	// Python: bits 48-63 = sigNameLen, bits 32-47 = payloadLen, bits 16-31 = timedOut, bits 0-15 = errCode
	// Go:     packAwaitSignalsResult has same layout
	sigNameLen := uint32(5)
	payloadLen := uint32(10)
	timedOut := false
	errCode := uint32(0)

	packed := packAwaitSignalsResult(sigNameLen, payloadLen, timedOut, errCode)

	r := uint64(packed)
	decodedSigNameLen := (r >> 48) & 0xFFFF
	decodedPayloadLen := (r >> 32) & 0xFFFF
	decodedTimedOut := ((r >> 16) & 0xFFFF) != 0
	decodedErrCode := r & 0xFFFF

	if decodedSigNameLen != uint64(sigNameLen) {
		t.Errorf("sigNameLen mismatch: Go packed %d, Python decodes %d", sigNameLen, decodedSigNameLen)
	}
	if decodedPayloadLen != uint64(payloadLen) {
		t.Errorf("payloadLen mismatch: Go packed %d, Python decodes %d", payloadLen, decodedPayloadLen)
	}
	if decodedTimedOut != timedOut {
		t.Errorf("timedOut mismatch: Go packed %v, Python decodes %v", timedOut, decodedTimedOut)
	}
	if decodedErrCode != uint64(errCode) {
		t.Errorf("errCode mismatch: Go packed %d, Python decodes %d", errCode, decodedErrCode)
	}
}

func testBitPackingSimpleResult(t *testing.T) {
	// Verify simple result bit packing (used by defer, child_workflow, etc.).
	// Python: bits 32-63 = extra, bits 0-7 = errCode
	// Go:     packSimpleResult has same layout
	errCode := byte(0)
	extra := uint32(100)

	packed := packSimpleResult(errCode, extra)

	r := uint64(packed)
	decodedExtra := (r >> 32) & 0xFFFFFFFF
	decodedErrCode := r & 0xFF

	if decodedExtra != uint64(extra) {
		t.Errorf("extra mismatch: Go packed %d, Python decodes %d", extra, decodedExtra)
	}
	if decodedErrCode != uint64(errCode) {
		t.Errorf("errCode mismatch: Go packed %d, Python decodes %d", errCode, decodedErrCode)
	}
}

func testBitPackingExportResult(t *testing.T) {
	// Verify export result bit packing.
	// Python: encode_export_result(err_code, actual_len) -> bits 0-31 = errCode, bits 32-63 = actualLen
	// Go:     decodeExportResult same layout
	errCode := uint32(0)
	actualLen := uint32(50)

	// Simulate Go's writeJSONOut encoding: (int64(actualLen) << 32) | errCode
	packed := uint64(actualLen)<<32 | uint64(errCode)

	decodedErrCode := uint32(packed & 0xFFFFFFFF)
	decodedActualLen := uint32(packed >> 32)

	if decodedErrCode != errCode {
		t.Errorf("errCode mismatch: %d vs %d", errCode, decodedErrCode)
	}
	if decodedActualLen != actualLen {
		t.Errorf("actualLen mismatch: %d vs %d", actualLen, decodedActualLen)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// pythonWasmTestHelper manages the Python WASM compilation toolchain.
type pythonWasmTestHelper struct {
	t        *testing.T
	repoRoot string
	sdkRoot  string
	missing  []string
}

func newPythonWasmTestHelper(t *testing.T) *pythonWasmTestHelper {
	h := &pythonWasmTestHelper{t: t}
	h.repoRoot = findRepoRoot(t)
	h.sdkRoot = filepath.Join(h.repoRoot, "python-sdk")
	h.missing = nil
	return h
}

func (h *pythonWasmTestHelper) toolsAvailable() bool {
	h.missing = nil

	// Check componentize-py.
	if _, err := exec.LookPath("componentize-py"); err != nil {
		h.missing = append(h.missing, "componentize-py (pip install componentize-py)")
	}

	// Check wasm-tools (needed to decompose component model -> core module).
	if _, err := exec.LookPath("wasm-tools"); err != nil {
		h.missing = append(h.missing, "wasm-tools (cargo install wasm-tools)")
	}

	// Check Python 3.
	if _, err := exec.LookPath("python3"); err != nil {
		h.missing = append(h.missing, "python3")
	}

	// Check that the SDK examples directory exists.
	examplesDir := filepath.Join(h.sdkRoot, "examples")
	if _, err := os.Stat(examplesDir); os.IsNotExist(err) {
		h.missing = append(h.missing, "python-sdk/examples/")
	}

	return len(h.missing) == 0
}

func (h *pythonWasmTestHelper) missingTools() string {
	return "missing: " + strings.Join(h.missing, ", ")
}

// compileWorkflow compiles a Python workflow to WASM using componentize-py.
func (h *pythonWasmTestHelper) compileWorkflow(t *testing.T, pyFile, funcName string) string {
	t.Helper()

	examplesDir := filepath.Join(h.sdkRoot, "examples")
	entry := filepath.Join(examplesDir, pyFile) + ":" + funcName

	output := filepath.Join(t.TempDir(), funcName+".wasm")

	// Use the cleat build_python.go path through BuildPythonWasm.
	cmd := exec.Command("python3",
		filepath.Join(h.sdkRoot, "scripts", "build_wasm.py"),
		"--entry", entry,
		"--output", output,
		"--verbose",
	)
	cmd.Dir = h.sdkRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+h.sdkRoot)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("componentize-py failed:\n%s\n%v", string(out), err)
	}
	t.Logf("componentize-py output:\n%s", string(out))

	if _, err := os.Stat(output); os.IsNotExist(err) {
		t.Fatalf("WASM output not found at %s", output)
	}
	return output
}

// canDecompose returns true if wasm-tools component decompose is available.
func (h *pythonWasmTestHelper) canDecompose() bool {
	wt, err := exec.LookPath("wasm-tools")
	if err != nil {
		return false
	}
	// Check that "wasm-tools component decompose" is a recognized subcommand.
	cmd := exec.Command(wt, "component", "decompose", "--help")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// decomposeComponent converts a Component Model binary to a core WASM module.
// Returns the path to the core module. If wasm-tools is not available or the
// binary is already a core module, returns the original path.
func (h *pythonWasmTestHelper) decomposeComponent(t *testing.T, wasmPath string) string {
	t.Helper()

	if _, err := exec.LookPath("wasm-tools"); err != nil {
		t.Log("wasm-tools not available, skipping component decomposition")
		return wasmPath
	}

	// Component model binaries start with \x00asm, but core modules also do.
	// The real test is whether CompileModule succeeds.
	// We attempt decomposition; if it fails we assume it's already a core module.
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "core.wasm")

	cmd := exec.Command("wasm-tools", "component", "decompose", wasmPath, "-o", outputPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("wasm-tools decompose failed (may already be core module): %s\n%s", err, string(out))
		return wasmPath
	}

	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		t.Log("decompose produced no output, using original")
		return wasmPath
	}

	return outputPath
}

// findRepoRoot locates the repository root by finding go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for dir := cwd; ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found from %s", cwd)
		}
	}
}

// pythonUnboundBaseline is the set of host functions the engine registers that
// the Python SDK does not bind. SHRINK-ONLY: an entry may be removed when the
// binding lands, and adding one requires a reason here.
//
// Measured 2026-09-07. The first three are not gaps and never will be; the rest
// are.
var pythonUnboundBaseline = []string{
	// Not workflow-facing. The worker handshake -- a guest never calls these,
	// the runtime does. CLAUDE.md names this pair explicitly when warning that
	// the export total and the workflow-facing total are different questions.
	"cleat_complete",
	"cleat_poll_work",

	// Deliberately unbindable. See docs/determinism.md, "Why there is no
	// RegisterQueryHandler" -- no engine version ever routed an external query
	// to it. Every SDK carries a comment saying it is absent on purpose.
	"cleat_register_query_handler",

	// Not a gap either, and this took a correction to see. cleat_json_parse and
	// cleat_json_stringify are covered by the `json` module -- host_calls.py
	// imports it three times -- and engine/lifecycle.go's JsonParse and
	// JsonStringify are pure: unmarshal, re-marshal, write, with no recordEvent
	// and no store. A guest using its own JSON diverges from nothing. Identical
	// reasoning to Go's two, which IMPROVEMENT-PLAN 3.241 applied to Go and then
	// explicitly denied for Python.
	"cleat_json_parse",
	"cleat_json_stringify",

	// A real gap, and the only one left. Blocked on a public API decision
	// rather than on wiring: Python already exports run_detached(fn), a CLOSURE
	// form that calls fn(self) inline and makes no host call, while promising
	// the host keeps the work alive past cancellation. That is Go's defect
	// exactly (3.244), and the closure form has real callers, so the exported
	// signature has to be decided rather than changed in passing. See 3.252.
	"cleat_run_detached",
}

// NOTE: this baseline and sdkUnreachedBaseline in
// tests/plugin-harness/sdk_import_names_test.go record the same fact from two
// different sources -- this one from python-sdk/cleat_sdk/host_calls.py, that
// one from wasm/component_rewrite.go's WitToEnvImport. THEY MOVE TOGETHER.
// 3.252 updated the harness one and not this one, and CI caught it; the same
// shape as the skip ledger's test-go/engine and cluster pair, which cost a
// round trip on #919 for the same reason.

// pythonSDKHostImports returns the host function names the Python SDK binds,
// read out of python-sdk/cleat_sdk/host_calls.py.
//
// # Why this is read twice, on purpose
//
// A name scan over source cannot tell a thing from a sentence about the thing.
// host_calls.py contains, at line 1031, the comment
//
//	# There is no _import_cleat_send_signal_and_wait or
//	# _import_cleat_reply_to_signal here (removed 2026-09-06, ...)
//
// -- an explicit denial that a careless scan reads as a confirmation, which is
// exactly how the AssemblyScript surface was once scored as HAVING a binding it
// had removed (IMPROVEMENT-PLAN 3.213).
//
// So the file is read two ways that go wrong in opposite directions: a strict
// line-anchored parse that only accepts an import alias where one can appear,
// and a deliberately loose scan that matches the alias token ANYWHERE, comments
// included. The loose one is wrong by construction; its only job is to disagree.
// Both readings agreeing is evidence. The strict one alone is a claim.
//
// Verified 2026-09-07 against a third reading -- Python's own `ast` module over
// the same file, collecting `alias.asname` -- which returned the identical 44.
func pythonSDKHostImports(t *testing.T) []string {
	t.Helper()

	path := filepath.Join(findRepoRoot(t), "python-sdk", "cleat_sdk", "host_calls.py")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the Python SDK host calls: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	// Strict: an alias binding, at a position where one can legally appear.
	// A comment cannot match this, because a comment line begins with '#'.
	strictRe := regexp.MustCompile(`^(?:from\s+\S+\s+import\s+)?\s*[A-Za-z_][A-Za-z0-9_]*\s+as\s+_import_([A-Za-z0-9_]+),?\s*$`)
	strict := map[string]bool{}
	for _, line := range lines {
		if m := strictRe.FindStringSubmatch(line); m != nil {
			strict[m[1]] = true
		}
	}

	// Loose: the alias token anywhere at all, prose included.
	looseRe := regexp.MustCompile(`\bas\s+_import_([A-Za-z0-9_]+)`)
	loose := map[string]bool{}
	for _, m := range looseRe.FindAllStringSubmatch(string(src), -1) {
		loose[m[1]] = true
	}

	// An extractor that sees less inflates every metric derived from it, and
	// nothing else in this test would notice: with zero names, "every Python
	// import is registered" is vacuously true. rust_surface() scored SDK
	// coverage at 100% while missing ten of seventy-one methods for want of
	// this check (IMPROVEMENT-PLAN 3.213).
	if len(strict) < 40 {
		t.Fatalf("the strict parse of %s found only %d import aliases, which is far below the "+
			"44 measured on 2026-09-07. The extractor has stopped matching, and every "+
			"assertion built on it is now vacuous rather than failing.", path, len(strict))
	}

	var onlyStrict, onlyLoose []string
	for n := range strict {
		if !loose[n] {
			onlyStrict = append(onlyStrict, n)
		}
	}
	for n := range loose {
		if !strict[n] {
			onlyLoose = append(onlyLoose, n)
		}
	}
	sort.Strings(onlyStrict)
	sort.Strings(onlyLoose)
	if len(onlyStrict) > 0 || len(onlyLoose) > 0 {
		t.Fatalf("the two readings of %s disagree, so neither can be trusted:\n"+
			"  only the strict (declaration-anchored) parse saw: %v\n"+
			"  only the loose (matches prose too) scan saw:      %v\n\n"+
			"A name appearing only in the loose scan is usually a comment ABOUT a binding -- "+
			"often one that was removed -- which must not be counted as a binding. A name "+
			"appearing only in the strict parse means the alias moved to a form the loose "+
			"scan cannot see, which should be impossible and means the file changed shape.",
			path, onlyStrict, onlyLoose)
	}

	out := make([]string, 0, len(strict))
	for alias := range strict {
		out = append(out, pythonAliasToHostName(alias))
	}
	sort.Strings(out)
	return out
}

// pythonAliasToHostName maps a Python import alias to the host function name.
//
// The convention is `_import_<hostname>`, but six aliases drop the `cleat_`
// prefix -- _import_uuid, _import_fetch, _import_side_effect, _import_get_scope,
// _import_set_scope, _import_continue_as_new_versioned -- so the alias is NOT
// the ABI name and treating it as one silently reports six phantom gaps.
//
// The rule is validated by its own output rather than asserted: applied to all
// 44 aliases it lands every one of them on a real engine export, with nothing
// missing. A wrong rule would produce names that resolve to nothing, and the
// `missing` check above is what would report it.
func pythonAliasToHostName(alias string) string {
	if strings.HasPrefix(alias, "cleat_") || strings.HasPrefix(alias, "plugin_") {
		return alias
	}
	if alias == "set_query_state" {
		return alias
	}
	return "cleat_" + alias
}

// setDiff returns the members of a that are not in b.
func setDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// compile-time check: engine.go must have the new HostHandler methods
// ---------------------------------------------------------------------------

// Ensure the execSession implements the full HostHandler interface.
var _ HostHandler = (*execSession)(nil)
