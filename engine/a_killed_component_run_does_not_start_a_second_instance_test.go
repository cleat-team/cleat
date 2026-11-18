//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// cleat#4079. After a fence kill the engine used to run a per-defer pass that, for each registered defer, asked the backend to
// execute the entry point "cleat_defer_<id>". On the component path that is not a defer drain: it instantiates a FRESH component
// (a full CPython start for Python) and calls `run` with an entry name no guest honours, and the defer's closure lives in the
// instance that was killed, which cannot be re-entered after a trap. So it never ran a defer, cost a second instance start per
// defer, and for a guest that ignores the entry key it executes the workflow body again.
//
// What is asserted is what the BACKEND was asked to do, because a second instance leaves no trace anywhere else: the engine's
// caller sees nothing (the fallback passes no handler, so a body it re-ran could not reach the ServiceCaller). A counting
// backend records every entry point the engine executes.
//
// The precondition matters as much as the assertion: the fallback only runs when the killed run registered a defer. A fixture
// that registered none would pass this test whether or not the fallback existed, so the test requires the defer, and requires
// the kill.

// recordingBackend wraps a WasmBackend and records every entry point executed through it or through its PerExecution copies.
type recordingBackend struct {
	WasmBackend
	rec *entryRecorder
}

type entryRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *entryRecorder) add(entry string) {
	r.mu.Lock()
	r.entries = append(r.entries, entry)
	r.mu.Unlock()
}

func (r *entryRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.entries...)
}

func (b *recordingBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	b.rec.add(entryPoint)
	return b.WasmBackend.Execute(ctx, wasmBytes, entryPoint, input, session)
}

func (b *recordingBackend) PerExecution(tenantInstanceTimeout time.Duration) WasmBackend {
	return &recordingBackend{WasmBackend: b.WasmBackend.PerExecution(tenantInstanceTimeout), rec: b.rec}
}

func TestAKilledComponentRunDoesNotStartASecondInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	wasmBytes, err := os.ReadFile(h.compileWorkflow(t, "spin_defer_workflow.py", "spin_defer_workflow"))
	if err != nil {
		t.Fatalf("read the compiled fixture: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	rec := &entryRecorder{}
	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, &recordingBackend{WasmBackend: wt, rec: rec}), WithWorkflowID("wf-kill-no-second-instance"))
	_, history, _, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(`{"request":{}}`))

	// PRECONDITIONS: it was killed, and it had registered a defer. Without both, nothing below exercises the post-kill pass.
	if err == nil {
		t.Fatal("PRECONDITION FAILED: the spinning workflow was not killed, so no post-kill pass ran")
	}
	// From the history, not Execute's deferrals return, which is empty on the error path.
	if len(DeferralsFromHistory(history)) == 0 {
		t.Fatalf("PRECONDITION FAILED: the killed run's history holds no defer registration (err: %v), so the post-kill defer pass is never reached", err)
	}

	got := rec.snapshot()
	if len(got) != 1 || got[0] != "run" {
		t.Fatalf("after the kill the backend was asked to execute %v, want exactly [run].\n\n"+
			"Any further entry point is the legacy cleat_defer_<id> pass: it starts a second component instance that cannot "+
			"hold the killed run's defer closures, so it runs nothing, and a guest that ignores the entry key would run its body "+
			"again. cleat#4079.", got)
	}
	if ops := operationsCalled(caller); len(ops) != 0 {
		t.Fatalf("the ServiceCaller saw %v after a kill in a loop that makes no calls", ops)
	}
}
