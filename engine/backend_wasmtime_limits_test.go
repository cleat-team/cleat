//go:build cgo

// Tests for IMPROVEMENT-PLAN.md 1.5: the wasmtime backend previously had no
// way to bound execution (bare wasmtime.NewEngine(), no epoch interruption,
// no fuel, no StoreLimits), so a runaway workflow — including one stuck in
// an infinite loop that never calls back into the host — hung the worker
// permanently. Before that fix, TestWasmtimeBackend_InfiniteLoop_GoStartPath
// and TestWasmtimeBackend_InfiniteLoop_DirectExportPath below would hang
// forever; they now assert the worker regains control within a bounded
// time and gets a clear, actionable error.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// infiniteLoopGoStartWat is a hand-written WASM module (no wasi imports, no
// component-model / python markers) that wasm.DetectLanguage classifies as
// "go" by default, exercising the same _start dispatcher code path
// (wasmtimeBackend.Execute's `if lang == "go" { ... startFn.Call(store) ...
// }` branch) that cmd/cleat-worker registers the wasmtime backend for (see
// engine.WithBackend("go", w.wasmtimeBackend) in cmd/cleat-worker/setup.go).
// _start loops forever and never calls the cleat_complete host import, so a
// correct fix must classify the resulting trap as a real error rather than
// falling through to the "no result recorded, assume success" path.
const infiniteLoopGoStartWat = `
(module
  (memory (export "memory") 1)
  (func (export "_start")
    (loop $inf
      br $inf))
)
`

// infiniteLoopDirectExportWat is the same infinite loop, but exported under
// a name other than "_start" with the (i32,i32,i32,i32)->i64 signature the
// backend uses for the generic direct-export-call path (non-Go / no _start
// modules). This exercises the fn.Call(...) branch further down
// wasmtimeBackend.Execute, independently of the _start branch above.
const infiniteLoopDirectExportWat = `
(module
  (memory (export "memory") 1)
  (func (export "run") (param i32 i32 i32 i32) (result i64)
    (loop $inf
      br $inf)
    unreachable)
)
`

// quickCompleteGoStartWat is a "go"-classified module whose _start calls
// the cleat_complete host import with a small JSON result and returns
// normally (no trap). Used to confirm that enabling epoch interruption,
// fuel, and StoreLimits does not break a legitimate, fast-completing
// workflow.
const quickCompleteGoStartWat = `
(module
  (import "env" "cleat_complete" (func $cleat_complete (param i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 2048) "\"done\"")
  (func (export "_start")
    (drop (call $cleat_complete (i32.const 0) (i32.const 2048) (i32.const 6))))
)
`

// mustWat2Wasm compiles WAT source to a WASM binary, failing the test on
// error (all WAT literals above are validated once in this file; if one of
// them stops compiling against a future wasmtime-go upgrade, this should
// fail loudly rather than silently skip the test it backs).
func mustWat2Wasm(t *testing.T, wat string) []byte {
	t.Helper()
	b, err := wasmtime.Wat2Wasm(wat)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}
	return b
}

// TestWasmtimeBackend_InfiniteLoop_GoStartPath is the mandatory regression
// test for IMPROVEMENT-PLAN.md 1.5: a workflow containing an infinite loop,
// run through the wasmtime backend's "go" _start dispatch path (the path
// cmd/cleat-worker actually registers the wasmtime backend for), must not
// hang the worker forever. Before the fix:
//   - NewWasmtimeBackend built a bare wasmtime.NewEngine() with no epoch
//     interruption, no fuel, and no deadline of any kind, so fn.Call (via
//     startFn.Call(store) here) would simply never return.
//   - Even if it had returned, the _start branch discarded startFn.Call's
//     error entirely and fell through to `return &ExecResult{Result:
//     `"ok"`, ...}` whenever cleat_complete hadn't been called — so a
//     killed-mid-loop execution would have been misreported as a
//     successful "ok" result instead of an error.
//
// This test uses a short configured timeout so it runs quickly, and relies
// on the test process's own goroutine + channel select (not go test
// -timeout) to turn "hung forever" into a reported failure rather than a
// suite-wide timeout panic.
func TestWasmtimeBackend_InfiniteLoop_GoStartPath(t *testing.T) {
	ctx := context.Background()
	const boundedTimeout = 200 * time.Millisecond

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(boundedTimeout))
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	wasmBytes := mustWat2Wasm(t, infiniteLoopGoStartWat)

	type outcome struct {
		res *ExecResult
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := b.Execute(ctx, wasmBytes, "_start", json.RawMessage(`{}`), &mockHostHandler{})
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		elapsed := time.Since(start)
		// Prove boundedness: the call returned well within a small
		// multiple of the configured timeout, not "eventually" via
		// go test's own -timeout killing the process.
		if elapsed > 5*time.Second {
			t.Errorf("Execute took %s to return after a %s configured timeout — not bounded", elapsed, boundedTimeout)
		}
		t.Logf("Execute returned after %s (configured timeout %s)", elapsed, boundedTimeout)

		if o.err == nil {
			t.Fatalf("Execute returned no error for an infinite loop (result=%+v) — the resource limit must surface as an error, not a silent success", o.res)
		}
		msg := o.err.Error()
		if !strings.Contains(msg, "execution time limit exceeded") {
			t.Errorf("error message %q does not mention the time limit that was hit", msg)
		}
		if !strings.Contains(msg, `"_start"`) {
			t.Errorf("error message %q does not name the entry point", msg)
		}
		var trap *wasmtime.Trap
		if !errors.As(o.err, &trap) {
			t.Errorf("expected the error to wrap a *wasmtime.Trap, got %T: %v", o.err, o.err)
		} else if code := trap.Code(); code == nil || *code != wasmtime.Interrupt {
			t.Errorf("expected trap code Interrupt, got %v", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return within 10s — the worker would hang forever on this workflow (the exact bug IMPROVEMENT-PLAN.md 1.5 describes)")
	}
}

// TestWasmtimeBackend_InfiniteLoop_DirectExportPath is the same regression
// test as above, but for the generic direct-export-call path (module with
// no _start, calling fn.Call(store, inputOffset, inputLen, outputOffset,
// outBufSz) directly).
func TestWasmtimeBackend_InfiniteLoop_DirectExportPath(t *testing.T) {
	ctx := context.Background()
	const boundedTimeout = 200 * time.Millisecond

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(boundedTimeout))
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	wasmBytes := mustWat2Wasm(t, infiniteLoopDirectExportWat)

	type outcome struct {
		res *ExecResult
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := b.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`), &mockHostHandler{})
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		elapsed := time.Since(start)
		if elapsed > 5*time.Second {
			t.Errorf("Execute took %s to return after a %s configured timeout — not bounded", elapsed, boundedTimeout)
		}
		if o.err == nil {
			t.Fatalf("Execute returned no error for an infinite loop (result=%+v)", o.res)
		}
		if !strings.Contains(o.err.Error(), "execution time limit exceeded") {
			t.Errorf("error message %q does not mention the time limit that was hit", o.err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return within 10s — worker hang bug reproduced")
	}
}

// TestWasmtimeBackend_InstructionLimit_FuelExhaustion exercises the
// secondary, opt-in bound: fuel-based instruction metering
// (--wasm-instruction-limit, now backend-agnostic — see
// cmd/cleat-worker/config.go). The execution timeout is set generously
// high so fuel exhaustion, not epoch interruption, is what stops the loop.
func TestWasmtimeBackend_InstructionLimit_FuelExhaustion(t *testing.T) {
	ctx := context.Background()

	b, err := NewWasmtimeBackend(ctx,
		WithWasmtimeExecutionTimeout(30*time.Second),
		WithWasmtimeInstructionLimit(10_000),
	)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	wasmBytes := mustWat2Wasm(t, infiniteLoopDirectExportWat)

	type outcome struct {
		res *ExecResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := b.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`), &mockHostHandler{})
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatalf("Execute returned no error for a fuel-exhausted infinite loop (result=%+v)", o.res)
		}
		if !strings.Contains(o.err.Error(), "instruction limit exceeded") {
			t.Errorf("error message %q does not mention the instruction limit that was hit", o.err.Error())
		}
		var trap *wasmtime.Trap
		if errors.As(o.err, &trap) {
			if code := trap.Code(); code == nil || *code != wasmtime.OutOfFuel {
				t.Errorf("expected trap code OutOfFuel, got %v", code)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return within 10s on a fuel-limited infinite loop")
	}
}

// TestWasmtimeBackend_NormalExecution_StillCompletes guards against the
// obvious failure mode of the fix above: a limit set too aggressively
// (or wired up incorrectly) breaking legitimate, fast-completing
// workflows. It runs the same limits configuration cmd/cleat-worker uses
// by default against a module that calls cleat_complete immediately.
func TestWasmtimeBackend_NormalExecution_StillCompletes(t *testing.T) {
	ctx := context.Background()

	b, err := NewWasmtimeBackend(ctx) // built-in defaults, same as an unconfigured worker
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	wasmBytes := mustWat2Wasm(t, quickCompleteGoStartWat)

	res, err := b.Execute(ctx, wasmBytes, "_start", json.RawMessage(`{}`), &mockHostHandler{})
	if err != nil {
		t.Fatalf("Execute failed for a normal, fast-completing workflow: %v", err)
	}
	if res == nil || res.Result != `"done"` {
		t.Errorf("Execute result = %+v, want Result=%q", res, `"done"`)
	}
}
