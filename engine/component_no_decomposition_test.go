//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// The decomposition path is gone; the native Component Model path is the only
// one. These tests pin what a component now meets on each of the two routes
// that used to fall through to decomposition.
//
// Measured 2026-09-01 before the deletion, against the only Component Model
// binary in the repo -- a 19.3 MB componentize-py build -- with the native path
// as the control:
//
//	native (ExecuteComponentCGo)  reached CPython, ran guest code, and returned
//	                              the guest's own "expected string, found bool"
//	                              from a deliberately wrong input -- i.e. it works
//	wasmtime decomposition        failed at instance 81 of 85: "incompatible
//	                              import type for env::cleat_call"
//	wazero decomposition          failed at instance 8: "memory is not exported
//	                              in module env"
//
// Two implementations, ~1,900 lines between them, failing at different points
// on a binary the remaining path executes. IMPROVEMENT-PLAN 3.65.

// componentWithNoImports is the smallest thing that is unambiguously a
// Component Model binary and can actually instantiate: a core module with a
// memory and a realloc, lifted to a `(param string) (result string)` export
// named "run", which is the shape python-sdk/wit/cleat.wit's world declares.
const componentWithNoImports = `(component
  (core module $M
    (memory (export "memory") 1)
    (func (export "realloc") (param i32 i32 i32 i32) (result i32) (i32.const 1024))
    (func (export "run") (param i32 i32) (result i32)
      (i32.store (i32.const 2048) (i32.const 4096))
      (i32.store (i32.const 2052) (i32.const 0))
      (i32.const 2048))
  )
  (core instance $i (instantiate $M))
  (func $lifted (param "input" string) (result string)
    (canon lift (core func $i "run")
      (memory $i "memory")
      (realloc (func $i "realloc"))
      string-encoding=utf8))
  (export "run" (func $lifted))
)`

// TestComponentOnTheBackendTakesTheNativePathOnly is the control that makes the
// two error assertions below mean something.
//
// Without it, "a component no longer reaches decomposition" would be satisfied
// by a backend that had stopped running components at all -- which is the most
// likely way to break this deletion, and the change would look like a success.
func TestComponentOnTheBackendTakesTheNativePathOnly(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := wasmtime.Wat2Wasm(componentWithNoImports)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}
	if !isComponentWasm(wasmBytes) {
		t.Fatal("the probe is not a Component Model binary, so it would not reach the " +
			"component branch at all and every assertion here would be vacuous")
	}

	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	res, err := b.PerExecution().Execute(ctx, wasmBytes, "run", json.RawMessage(`"x"`), &mockHostHandler{})
	if err != nil {
		t.Fatalf("Execute on a Component Model binary: %v\n\n"+
			"The native Component Model path is the only one left. If components stop "+
			"running here they do not run anywhere -- python is a tier 1 language.", err)
	}
	if res == nil {
		t.Fatal("Execute returned no result and no error for a component")
	}
}

// TestComponentFailureIsNotFollowedByASecondWorseError pins what the deletion
// changed for a caller.
//
// A native-path failure used to be a prelude: Execute logged it and then ran
// decomposition, whose own failure was what the caller actually saw. That
// second error described decomposition's problems assembling the module --
// unresolved imports, incompatible import types -- and read like "wasmtime
// cannot run this component" when the real cause was something else entirely.
// IMPROVEMENT-PLAN 2.72 records months of that error being read as wasmtime's
// verdict on Component Model guests.
//
// The assertion is that the error names the entry point that was actually
// looked for, and does not mention the vocabulary decomposition failed in.
func TestComponentFailureIsNotFollowedByASecondWorseError(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := wasmtime.Wat2Wasm(componentWithNoImports)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	_, err = b.PerExecution().Execute(ctx, wasmBytes, "no-such-export", json.RawMessage(`"x"`), &mockHostHandler{})
	if err == nil {
		t.Fatal("Execute succeeded for an entry point the component does not export")
	}
	if !strings.Contains(err.Error(), "no-such-export") {
		t.Errorf("the error does not name the entry point that was looked for: %v", err)
	}
	// "instance" and "import type" are decomposition's vocabulary: it failed at
	// "instantiate instance 81 ... incompatible import type for env::cleat_call".
	for _, leaked := range []string{"instantiate instance", "incompatible import type", "component bundle"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the error contains %q, which is decomposition's vocabulary: %v\n\n"+
				"A native-path failure must be the answer, not a prelude to a second and "+
				"less accurate one.", leaked, err)
		}
	}
}

// TestComponentWithoutABackendSaysHowToRunIt covers the other route that used
// to decompose: engine.Execute on an engine with a wazero Runtime and no
// backends -- cleatctl replay|debug, cleat run_embedded, cleat-bench,
// cleat/wasmtest.
//
// That route decomposed onto wazero and failed at instance 8 with "memory is
// not exported in module env", which tells an operator nothing they can act on.
// The message must now name the fix, because for this engine shape there is a
// real one: register the wasmtime backend.
func TestComponentWithoutABackendSaysHowToRunIt(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := wasmtime.Wat2Wasm(componentWithNoImports)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{})
	if len(eng.backends) != 0 {
		t.Fatalf("expected an engine with no backends, got %d", len(eng.backends))
	}

	_, _, _, _, _, err = eng.Execute(ctx, wasmBytes, "run", json.RawMessage(`"x"`))
	if err == nil {
		t.Fatal("Execute succeeded for a component on an engine with no backend.\n\n" +
			"There is no longer an implementation that could have run it, so a nil error " +
			"means something returned success without executing anything.")
	}
	msg := err.Error()
	for _, want := range []string{"Component Model", "WithBackend"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q: %v\n\n"+
				"It replaced \"memory is not exported in module env\" from instance 8 of "+
				"the wazero decomposition, which named neither the problem nor the fix.", want, err)
		}
	}
	if strings.Contains(msg, "is not exported in module") {
		t.Errorf("the error is still decomposition's instantiation failure: %v", err)
	}
}
