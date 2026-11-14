//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// cleat#3786: a Python component built before the fix calls durable-await-signals
// and get-scope with output pointers, and cannot be served by a host that now
// returns values: it would be handed NULs for a signal's name and payload. It is
// refused on the call, BEFORE the host runs. wasmtime checks a dynamically-typed
// host function against the guest's declared signature only when it returns, so
// without the refusal the host has already polled the store, consumed the
// delivery and recorded a signal_received event by the time the mismatch is seen
// -- and the signal is lost to the rebuilt workflow too.
//
// The stale component is built, not stored: the current SDK is copied, its WIT
// has the two old declarations put back, and a fixture calls the generated
// binding with the old arity (engine/testdata/pre3786). Every replacement is
// asserted, so an SDK that drifts fails here rather than building something else.

const (
	staleSignalsOld = `    durable-await-signals: func(
        names: string,
        timeout-ms: u64,
        sig-name-ptr: u32,
        sig-name-max-len: u32,
        payload-ptr: u32,
        payload-max-len: u32,
    ) -> u64;
`
	staleScopeOld = `    get-scope: func(
        obj-type-ptr: u32,
        obj-type-max-len: u32,
        inst-key-ptr: u32,
        inst-key-max-len: u32,
    ) -> u64;
`
)

// replaceBetween swaps everything from the first occurrence of begin through the
// first following occurrence of end with repl, failing if either marker is absent.
func replaceBetween(t *testing.T, src, begin, end, repl string) string {
	t.Helper()
	i := strings.Index(src, begin)
	if i < 0 {
		t.Fatalf("marker %q not found; the SDK's WIT has changed under this test", begin)
	}
	j := strings.Index(src[i:], end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, begin)
	}
	return src[:i] + repl + src[i+j+len(end):]
}

func copySDKTree(t *testing.T, from, to string) {
	t.Helper()
	for _, sub := range []string{"cleat_sdk", "wit", "scripts"} {
		root := filepath.Join(from, sub)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(from, path)
			if d.IsDir() {
				if d.Name() == "__pycache__" {
					return fs.SkipDir
				}
				return os.MkdirAll(filepath.Join(to, rel), 0o755)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(to, rel), b, 0o644)
		})
		if err != nil {
			t.Fatalf("copy %s: %v", sub, err)
		}
	}
}

// buildStaleComponent compiles one entry of the pre-fix fixture against an SDK
// copy carrying the old WIT, and returns the component's bytes.
func buildStaleComponent(t *testing.T, entry string) []byte {
	t.Helper()
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}

	sdk := t.TempDir()
	copySDKTree(t, h.sdkRoot, sdk)

	witPath := filepath.Join(sdk, "wit", "cleat.wit")
	wit, err := os.ReadFile(witPath)
	if err != nil {
		t.Fatal(err)
	}
	w := string(wit)
	w = replaceBetween(t, w, "    /// A signal a wait received:", "    ) -> result<signal-outcome, call-failure>;\n", staleSignalsOld)
	w = replaceBetween(t, w, "    /// The virtual object scope a workflow is running under.", "    get-scope: func() -> scope;\n", staleScopeOld)
	if !strings.Contains(w, "sig-name-ptr") || !strings.Contains(w, "obj-type-ptr") || strings.Contains(w, "signal-outcome") {
		t.Fatal("the stale WIT was not produced")
	}
	if err := os.WriteFile(witPath, []byte(w), 0o644); err != nil {
		t.Fatal(err)
	}

	// host_calls.py imports the new record types by name, which the old WIT does
	// not generate; without this it would fall back to its stubs wholesale.
	hcPath := filepath.Join(sdk, "cleat_sdk", "host_calls.py")
	hc, err := os.ReadFile(hcPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(hc)
	for _, name := range []string{"SignalOutcome_Received as _SignalOutcomeReceived", "SignalOutcome_TimedOut as _SignalOutcomeTimedOut"} {
		blk := "    from wit_world.imports.durable_signals import (\n        " + name + ",\n    )\n"
		if strings.Count(src, blk) != 1 {
			t.Fatalf("import block for %s not found exactly once", name)
		}
		src = strings.Replace(src, blk, "", 1)
	}
	if err := os.WriteFile(hcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture, err := os.ReadFile(filepath.Join(h.repoRoot, "engine", "testdata", "pre3786", "signal_scope_workflow.py"))
	if err != nil {
		t.Fatal(err)
	}
	exDir := filepath.Join(sdk, "examples")
	if err := os.MkdirAll(exDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(exDir, "signal_scope_workflow.py")
	if err := os.WriteFile(fixturePath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), entry+".wasm")
	cmd := exec.Command("python3", filepath.Join(sdk, "scripts", "build_wasm.py"),
		"--entry", fixturePath+":"+entry, "--output", out, "--verbose")
	cmd.Dir = sdk
	cmd.Env = append(os.Environ(), "PYTHONPATH="+sdk)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("componentize-py failed against the stale SDK:\n%s\n%v", b, err)
	}
	bytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

// countingSignalStore wraps the scripted store and counts how often the host
// reached it. A host that never ran leaves every counter at zero.
type countingSignalStore struct {
	*scriptedSignalStore
	polls, consumes atomic.Int64
}

func (s *countingSignalStore) PollSignal(ctx context.Context, wf, name string) (SignalDelivery, bool, error) {
	s.polls.Add(1)
	return s.scriptedSignalStore.PollSignal(ctx, wf, name)
}

func (s *countingSignalStore) ConsumeSignal(ctx context.Context, wf string, id int64) error {
	s.consumes.Add(1)
	return s.scriptedSignalStore.ConsumeSignal(ctx, wf, id)
}

func TestStalePythonComponentCallingAwaitSignalsIsRefusedBeforeTheHostRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	wasmBytes := buildStaleComponent(t, "old_await_signals")

	store := &countingSignalStore{scriptedSignalStore: newScriptedSignalStore()}
	store.queue("go", `{"approved":true}`)
	_, eng := pythonEntryEngine(t, awaitSignalsFixture, "await_signal", &mockCaller{}, WithSignalStore(store))

	result, hist, _, _, _, err := eng.Execute(context.Background(), wasmBytes, "old_await_signals", json.RawMessage(`{}`))

	// The run must not have been served: not as a result, and not as an error
	// that merely mentions a type mismatch.
	text := ""
	if err != nil {
		text = err.Error()
	}
	if err == nil && !strings.Contains(result, "cleat#3786") {
		t.Fatalf("the stale component was served: result=%q", result)
	}
	if !strings.Contains(text+result, "cleat#3786") || !strings.Contains(text+result, "Rebuild the workflow against cleat >= 0.6.0") {
		t.Errorf("the refusal does not name the bug and the remedy.\nerr=%v\nresult=%q", err, result)
	}

	// BEFORE the host runs: no store access, no delivery consumed, no event.
	if n := store.polls.Load(); n != 0 {
		t.Errorf("the host polled the signal store %d time(s) for a component it should have refused", n)
	}
	if n := store.consumes.Load(); n != 0 {
		t.Errorf("the host consumed %d delivery(ies); the signal is lost to the rebuilt workflow", n)
	}
	if len(hist) != 0 {
		t.Errorf("history has %d event(s) %+v; a refused call must record nothing", len(hist), hist)
	}
	if d, found, _ := store.scriptedSignalStore.PollSignal(context.Background(), "", "go"); !found || d.Payload != `{"approved":true}` {
		t.Errorf("the queued signal is no longer pending (found=%v payload=%q)", found, d.Payload)
	}
}

func TestStalePythonComponentCallingGetScopeIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	wasmBytes := buildStaleComponent(t, "old_get_scope")
	_, eng := pythonEntryEngine(t, awaitSignalsFixture, "scope_roundtrip", &mockCaller{})

	result, hist, _, _, _, err := eng.Execute(context.Background(), wasmBytes, "old_get_scope", json.RawMessage(`{}`))
	text := result
	if err != nil {
		text = err.Error() + " " + result
	}
	if !strings.Contains(text, "cleat#3786") || !strings.Contains(text, "Rebuild the workflow against cleat >= 0.6.0") {
		t.Errorf("the refusal does not name the bug and the remedy.\nerr=%v\nresult=%q", err, result)
	}
	if strings.Contains(text, `"reached"`) {
		t.Errorf("the stale component ran past get-scope: %q", text)
	}
	if len(hist) != 0 {
		t.Errorf("history has %d event(s); a refused call must record nothing", len(hist))
	}
}
