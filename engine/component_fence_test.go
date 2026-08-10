//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The execution fence on the native Component Model path.
//
// IMPROVEMENT-PLAN 3.31 asks for the execution-limit story to be written and
// tested per *backend* rather than per language. This is the wasmtime
// backend's third execution path -- core modules, decomposition, and the
// native component path -- and it was the one that did not enforce the
// caller's budget.
// ---------------------------------------------------------------------------

// TestComponentPathResourceLimitClassification covers the classifier that
// makes the fence legible, in the form the component path produces.
//
// The two paths report the same trap differently. Core modules and
// decomposition come back through wasmtime-go as a *wasmtime.Trap with a
// machine-readable code. The native component path comes back through the
// Component Model C API, whose wasmtime_error_t exposes a rendered message, an
// exit status and a wasm trace -- and no trap code (wasmtimeinc/wasmtime/
// error.h). So that path is classified by matching wasmtime's own rendering,
// and this test is what stands between that and a silent regression if the
// wording changes upstream: the failure mode without it is not a wrong answer
// but a missing one -- an exhausted budget stops being recognised as a limit,
// which is exactly when Execute would resume falling back to decomposition and
// hand a runaway guest a second budget.
func TestComponentPathResourceLimitClassification(t *testing.T) {
	b := &wasmtimeBackend{}
	b.limits.instructionLimit = 5_000_000
	const budget = 2 * time.Second

	// The messages wasmtime_error_message renders, wrapped the way
	// componentCall wraps them, so the input to the classifier is the shape it
	// actually sees rather than a bare phrase.
	componentErr := func(trapText string) error {
		return fmt.Errorf("component call: error while executing at wasm backtrace:\n"+
			"    0: 0x11e5934 - componentize_py_runtime.wasm!wit_dylib_export_call\n"+
			"    1: 0x122751a - <unknown>!run\n\nCaused by:\n    %s", trapText)
	}

	tests := []struct {
		name      string
		err       error
		wantLimit bool
		wantText  string
	}{
		{
			name:      "epoch interrupt",
			err:       componentErr("wasm trap: interrupt"),
			wantLimit: true,
			wantText:  "execution time limit exceeded (2s wall-clock budget",
		},
		{
			name:      "fuel exhaustion",
			err:       componentErr("all fuel consumed by WebAssembly"),
			wantLimit: true,
			wantText:  "instruction limit exceeded (5000000 fuel units",
		},
		{
			// The guest failing on its own must not be mistaken for the host
			// stopping it: that direction is what decides whether Execute
			// falls back to decomposition, and suppressing a real fallback
			// would strand every component that legitimately needs one.
			name:      "guest fault is not a limit",
			err:       componentErr("wasm trap: undefined element: out of bounds table access"),
			wantLimit: false,
		},
		{
			name:      "unrelated error",
			err:       errors.New("component export \"run\" not found"),
			wantLimit: false,
		},
		{
			name:      "nil",
			err:       nil,
			wantLimit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.resourceLimitError(tt.err, budget)
			if !tt.wantLimit {
				if got != nil {
					t.Fatalf("resourceLimitError = %v, want nil (not a host-imposed limit)", got)
				}
				if isExecutionLimit(tt.err) {
					t.Errorf("isExecutionLimit(%v) = true, want false", tt.err)
				}
				return
			}
			if got == nil {
				t.Fatalf("resourceLimitError = nil, want a limit error naming the budget")
			}
			if !strings.Contains(got.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", got.Error(), tt.wantText)
			}
			// The marker is what Execute reads to decide not to fall back, so
			// producing the right message without it would be a half-fix.
			if !isExecutionLimit(got) {
				t.Errorf("isExecutionLimit = false for %v; Execute would fall back to "+
					"decomposition and hand the guest a second budget", got)
			}
			// The original must stay reachable: it carries the guest backtrace.
			if !errors.Is(got, tt.err) {
				t.Errorf("classified error no longer unwraps to the original")
			}
		})
	}
}

// TestPythonComponentExecutionFence is the end-to-end half, and the only thing
// that verifies the trap text above is really what wasmtime renders rather than
// what this repo believes it renders.
//
// It runs a Python component that never returns, under a budget far shorter
// than it would ever take, and asserts the budget is what stops it.
//
// Before the fix it did not: ExecuteComponentCGo passed context.Background()
// to configureStore, so the per-workflow deadline the executor puts on ctx was
// dropped and the guest got the backend-wide default instead. Measured on this
// exact workflow: a 2s budget ran for 32.9s. The fence fired -- on the wrong
// deadline -- which is why nothing looked broken from the outside.
func TestPythonComponentExecutionFence(t *testing.T) {
	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmPath := pythonWasm.compileWorkflow(t, "spin_workflow.py", "spin_workflow")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

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
	const budget = 2 * time.Second
	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithDefaultWorkflowTimeout(budget),
	)

	start := time.Now()
	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "run", json.RawMessage(`{"request":{}}`))
	elapsed := time.Since(start)

	if execErr == nil {
		t.Fatalf("a workflow that never returns completed successfully after %s; "+
			"the execution fence did not fire", elapsed)
	}

	// The primary assertion is on the message, not the clock. A timing
	// assertion is the thing this repo has already been bitten by (a 2 ms
	// sleep that survived four CI runs and lost the fifth), and the message
	// distinguishes the fixed and unfixed code exactly: unfixed, the budget
	// named here was the backend default, not the configured one.
	if !strings.Contains(execErr.Error(), "execution time limit exceeded") {
		t.Errorf("error does not name the execution time limit, so the fence is not what "+
			"stopped this: %v", execErr)
	}

	// Secondary, and deliberately loose. Its only job is to catch the specific
	// regression of falling back to the backend-wide default
	// (DefaultWasmtimeExecutionTimeout, 30s) instead of the 2s budget, so any
	// threshold well between the two works and there is nothing to tune. It is
	// not measuring how promptly the fence fires.
	if limit := DefaultWasmtimeExecutionTimeout / 2; elapsed > limit {
		t.Errorf("budget was %s but execution ran %s (over %s): the fence fired on some "+
			"deadline other than the caller's -- most likely the backend default of %s",
			budget, elapsed, limit, DefaultWasmtimeExecutionTimeout)
	}
}
