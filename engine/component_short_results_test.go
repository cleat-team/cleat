package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestComponentShortStringResultsAreNotTruncated is the regression test for a
// bug that made most string-returning host calls come back EMPTY to a Component
// Model guest.
//
// The bridge decoded every string result with extractStringFromPacked, which
// reads the length from bits 40-63 -- the layout packDurableCallResult uses.
// Nineteen of the twenty-four dispatchers call handlers that pack with
// packSimpleResult, whose length is at bits 32-63. Reading the wrong field
// shifts the length right by 8, so ANY result shorter than 256 bytes decoded as
// length 0. Longer ones came back truncated rather than empty, which is worse
// to diagnose.
//
// Why nothing caught it:
//
//   - The five dispatchers that WERE correct are the durable-call and plugin
//     paths, which is what every existing component test exercises.
//     TestPythonWasmEndToEnd calls h.call() and passed throughout.
//   - The unit-test mock packs n<<40 -- the same assumption the extractor makes,
//     not the one production uses. So the tests agreed with the bug.
//
// This test therefore goes through a real component: the fixture calls
// current_workflow_id() and current_run_id(), both of which return well under
// 256 bytes and both of which are on the broken side. It asserts each value
// survives, which a mock cannot fake and a log line would have hidden.
//
// uuid() would have been a third case and is deliberately not used: it has no
// WIT binding for Python, so it exercises the SDK's fallback stub rather than
// the bridge. Discovered by trying it -- the fixture came back with
// "uuid can only be called within a cleat WASM runtime".
func TestComponentShortStringResultsAreNotTruncated(t *testing.T) {
	ctx := context.Background()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmPath := pythonWasm.compileWorkflow(t, "short_results_workflow.py", "short_results_workflow")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	wt, wtErr := NewWasmtimeBackend(ctx)
	if wtErr != nil {
		t.Fatalf("NewWasmtimeBackend: %v (python routes here; there is no fallback to test)", wtErr)
	}
	engine := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt))

	result, _, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "run", json.RawMessage(`{"_unused":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}

	// The result arrives JSON-encoded; componentize-py hands back a JSON string.
	raw := strings.TrimSpace(result)
	var unquoted string
	if err := json.Unmarshal([]byte(raw), &unquoted); err == nil {
		raw = unquoted
	}

	parts := strings.Split(raw, "|")
	if len(parts) != 2 {
		t.Fatalf("result %q is not \"<workflow-id>|<run-id>\"; fixture and test have diverged", result)
	}

	for _, c := range []struct {
		call string
		got  string
	}{
		{"current_workflow_id", parts[0]},
		{"current_run_id", parts[1]},
	} {
		if c.got == "" {
			t.Errorf("%s() returned an empty string through the component boundary.\n"+
				"That is the signature of the bit-layout bug: the handler packs its length at "+
				"bit 32 (packSimpleResult) and the bridge read it from bit 40, so anything "+
				"under 256 bytes decodes as length 0. Check which extractor "+
				"dispatch%s uses.", c.call, strings.ToUpper(c.call[:1])+c.call[1:])
		}
	}

}
