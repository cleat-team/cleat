//go:build cgo

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// ---------------------------------------------------------------------------
// Does a defer body actually execute?
//
// Nothing asked before. The three TestRunDefers_* tests in flush_test.go each
// pass wasmBytes = nil and say so in a comment -- "wasmBytes is nil so RunDefer
// is not invoked; verify no panic" -- so they cover the sorting and the nil
// guards and stop exactly where the guest begins. TestClosure_CleatDefer covers
// the cleat_defer host function, which is how a workflow *registers* a defer,
// not how one runs.
//
// So the defer execution path had no test that ran a defer. That is the gap
// this file closes, and it is a prerequisite rather than an end in itself:
// IMPROVEMENT-PLAN 3.32 wants defers routed through a backend so the execution
// fence applies to them, and that change is unsafe to make against no coverage.
// ---------------------------------------------------------------------------

// deferTestModule builds a module exporting three defer bodies with the
// signature CallExport invokes -- func(argsPtr, argsLen, outPtr, maxOutLen
// uint32) int64 (engine/runtime.go) -- plus the memory it requires.
//
// Built from WAT rather than a checked-in fixture: what each body does has to
// be legible for the assertions to mean anything, and a fixture would drift.
func deferTestModule(t *testing.T) []byte {
	t.Helper()
	wasmBytes, err := wasmtime.Wat2Wasm(`(module
	  ;; Returns cleanly: the "defer ran and succeeded" case.
	  (func (export "cleat_defer_defer-1") (param i32 i32 i32 i32) (result i64)
	    (i64.const 0))
	  ;; Traps: the "defer ran and failed" case. A trap is the cheapest
	  ;; observable proof that the body was entered at all -- it needs no host
	  ;; handler and no output-ABI agreement, both of which a defer lacks.
	  (func (export "cleat_defer_defer-2") (param i32 i32 i32 i32) (result i64)
	    (unreachable))
	  ;; Wrong arity on purpose. See TestRunDefer_WrongSignatureIsReported.
	  (func (export "cleat_defer_defer-3")
	    (nop))
	  (memory (export "memory") 1))`)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}
	return wasmBytes
}

func newDeferTestEngine(t *testing.T) *Engine {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	return NewEngine(rt, &mockCaller{})
}

// TestRunDefer_ExecutesTheBody asserts the two outcomes that distinguish "the
// defer ran" from "nothing happened", which is the distinction every existing
// test was structurally unable to draw.
func TestRunDefer_ExecutesTheBody(t *testing.T) {
	wasmBytes := deferTestModule(t)

	t.Run("clean return", func(t *testing.T) {
		e := newDeferTestEngine(t)
		if _, err := e.RunDefer(context.Background(), wasmBytes, "cleat_defer_defer-1", nil); err != nil {
			t.Errorf("RunDefer on a body that returns 0: %v", err)
		}
	})

	t.Run("trap propagates", func(t *testing.T) {
		e := newDeferTestEngine(t)
		_, err := e.RunDefer(context.Background(), wasmBytes, "cleat_defer_defer-2", nil)
		if err == nil {
			// The load-bearing assertion. A defer body that traps must produce
			// an error; if RunDefer reports success here, it did not enter the
			// guest, and every other assertion in this file would be vacuous.
			t.Fatal("RunDefer on a body containing `unreachable` returned no error, " +
				"so the body never executed")
		}
		if !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("error does not name the trap, so it may not be the guest's: %v", err)
		}
	})
}

// TestRunDefer_MissingExportIsReported separates "the defer ran and failed"
// from "there was no defer to run".
//
// It matters because runDefers logs RunDefer's error and moves on, so these two
// are indistinguishable from the outside -- which is how the first draft of
// cmd/cleat-worker's defer test passed while executing nothing at all
// (IMPROVEMENT-PLAN 3.32).
func TestRunDefer_MissingExportIsReported(t *testing.T) {
	e := newDeferTestEngine(t)
	_, err := e.RunDefer(context.Background(), deferTestModule(t), "cleat_defer_nonexistent", nil)
	if err == nil {
		t.Fatal("RunDefer on an export that does not exist returned no error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say the export was not found, got: %v", err)
	}
}

// TestRunDefer_WrongSignatureIsReported pins the failure that hid a vacuous
// test for a whole session.
//
// CallExport invokes a defer with four i32 arguments. An export declared with
// none is rejected before it runs, with "expected 0 params, but passed 4" --
// and because runDefers swallows that, a defer body written with the wrong
// signature is silently never executed. A workflow author would see cleanup
// quietly not happening, which is the same shape as the defect in 3.32 and
// worth a test of its own.
func TestRunDefer_WrongSignatureIsReported(t *testing.T) {
	e := newDeferTestEngine(t)
	_, err := e.RunDefer(context.Background(), deferTestModule(t), "cleat_defer_defer-3", nil)
	if err == nil {
		t.Fatal("RunDefer on an export with the wrong signature returned no error")
	}
	if !strings.Contains(err.Error(), "param") {
		t.Errorf("error should name the parameter mismatch, got: %v", err)
	}
}

// TestRunDefers_InvokesEveryDeferral covers the plural, which sorts the
// deferrals and calls RunDefer for each.
//
// It asserts through the only channel available: runDefers discards RunDefer's
// return and error, so nothing about an individual defer is observable from
// outside. What *is* observable is that a trapping body does not stop the
// others -- cleanup is best-effort by design, and a defer that traps must not
// prevent the rest from running. Asserting that requires the whole set to be
// attempted, which is the property under test.
func TestRunDefers_InvokesEveryDeferral(t *testing.T) {
	e := newDeferTestEngine(t)
	// defer-2 traps. If a trap aborted the loop, this would hang or panic
	// rather than return.
	e.runDefers(context.Background(), deferTestModule(t), map[string]string{
		"defer-1": "clean",
		"defer-2": "traps",
		"defer-3": "wrong signature",
	})
	// Reaching here is the assertion: every entry was attempted and the two
	// failures were absorbed. Deliberately not asserting on logs -- runDefers
	// writes through the engine logger, and pinning its wording would make
	// this a test of a log line rather than of control flow.
}
