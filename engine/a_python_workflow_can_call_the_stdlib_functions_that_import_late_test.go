package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// cleat#4266. componentize-py keeps only the standard-library modules that were imported while the component was BUILT, so a call that imports
// another module on first use raised ModuleNotFoundError at run time, though the workflow's own imports were all at the top of the file:
// datetime.strptime and time.strptime (_strptime), Counter.most_common (heapq), and five codecs (their encodings modules). The SDK now imports
// them, and every workflow imports the SDK, so they are in the component.
//
// A REAL Python workflow built with the pinned componentize-py and run on wasmtime, because what is being tested is what the component CONTAINS,
// and nothing short of a built component can see that. The fixture does not import any of the modules itself.
func TestPythonWorkflowCanCallTheStdlibFunctionsThatImportLate(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}

	fixture := filepath.Join(h.repoRoot, "engine", "testdata", "pystdlib", "late_calls_workflow.py")
	out := filepath.Join(t.TempDir(), "late_calls.wasm")
	cmd := exec.Command("python3", filepath.Join(h.sdkRoot, "scripts", "build_wasm.py"), "--entry", fixture+":late_calls", "--output", out)
	cmd.Dir = h.sdkRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+h.sdkRoot)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture failed: %v\n%s", err, b)
	}
	wasmBytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the built component: %v", err)
	}
	t.Logf("built component: %.2f MiB", float64(len(wasmBytes))/(1<<20))

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)
	eng := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-late-calls"),
		WithWorkflowStartTime(1_000_000_000_000), WithClock(func() int64 { return 1_000_000_000_000 }))

	t0 := time.Now()
	result, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "late_calls", []byte(`{}`))
	t.Logf("first run (compile + instantiate + run): %v", time.Since(t0).Round(time.Millisecond))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]struct {
		OK    json.RawMessage `json:"ok"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("the result is not the expected JSON: %v\n%s", err, result)
	}

	// Decoded values, not text: the guest's json.dumps escapes non-ASCII and spaces its separators, and neither is what is being tested. These are
	// what CPython returns for these inputs, so a call that "worked" by returning something else fails too.
	want := map[string]any{
		"datetime.strptime":   "2026-11-23T05:06:07",
		"time.strptime":       float64(327),
		"Counter.most_common": []any{[]any{"a", float64(5)}, []any{"b", float64(2)}},
		"cp1252":              "\u20ac caf\u00e9",
		"shift_jis":           "\u65e5\u672c",
		"iso-8859-15":         "\u20ac 5",
		"utf-8-sig":           "hello",
		"idna":                "xn--bcher-kva.example",
	}
	if len(got) != len(want) {
		t.Fatalf("the workflow reported %d calls, want %d: %s", len(got), len(want), result)
	}
	for name, w := range want {
		r, ok := got[name]
		if !ok {
			t.Errorf("%s: no outcome reported", name)
			continue
		}
		if r.Error != "" {
			t.Errorf("%s failed at run time: %s", name, r.Error)
			continue
		}
		var v any
		if err := json.Unmarshal(r.OK, &v); err != nil || !reflect.DeepEqual(v, w) {
			t.Errorf("%s = %s, want %v", name, r.OK, w)
		}
	}
}
