//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// An entry point the guest does not recognise was reported as success.
//
// A Go guest does not have its exports called by name. The host runs _start,
// the guest asks cleat_poll_work what to run, and routes internally through
// cleatDispatch. That router's default case returned
//
//	{"error":"unknown entry point: <name>"}
//
// as its RESULT, and the generated main stub handed whatever it returned to
// cleatCompleteImport(0, ...) -- status 0, success. So the host received a
// successful execution whose result happened to contain the word "error", and
// every caller's `if err != nil` was dead code for this case.
//
// Measured 2026-09-01 against a real Go SDK guest, through the wasmtime
// backend:
//
//	RunDefer("cleat_defer_defer-0")        -> {"error":"unknown entry point: ..."} err=<nil>
//	RunDefer("totally_nonexistent_export") -> {"error":"unknown entry point: ..."} err=<nil>
//
// Byte-identical: a plausible-but-absent export could not be distinguished
// from a nonsense one, and neither from a real one that returned that JSON.
//
// This is how every defer in every Go WASM workflow came to do nothing while
// the host recorded success -- the host invokes defers by entry-point name and
// no guest exports one -- but the defect is not defer-specific. Any mis-routed
// entry point, from a typo in a workflow name to a stale definition, reported
// success and produced no log line anywhere.
//
// IMPROVEMENT-PLAN 3.70.
//
// Note the scope of the fix: it is codegen, so it takes effect when a guest is
// rebuilt. A workflow binary compiled before this change keeps the old
// behaviour, because the faulty branch is inside the guest.

// TestUnknownEntryPointIsAFailure builds a real Go SDK guest and asks it for
// an entry point it does not have.
func TestUnknownEntryPointIsAFailure(t *testing.T) {
	ctx := context.Background()
	wasmPath := buildFixtureWasm(t, "basic")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

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

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-unknown-entry"))

	result, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"no_such_entry_point", json.RawMessage(`{}`))

	if execErr == nil {
		t.Fatalf("asking the guest for an entry point it does not have reported "+
			"SUCCESS, with result %q.\n\n"+
			"The host routes by entry-point name and cannot otherwise tell a name "+
			"the guest never heard of from one that ran. That is why every defer "+
			"in every Go WASM workflow did nothing while the host recorded success "+
			"-- see IMPROVEMENT-PLAN 3.70.", result)
	}
	if !strings.Contains(execErr.Error(), "no_such_entry_point") {
		t.Errorf("the failure does not name the entry point that was not found: %v", execErr)
	}
}

// TestKnownEntryPointStillSucceeds is the control, and it is not a formality.
//
// "An unknown entry point fails" is trivially satisfied by a guest that fails
// for every entry point -- which is the most likely way to break this change,
// since it edits the dispatcher's default arm and the stub that completes
// after it. If this test and the one above cannot both hold, the fix is wrong.
func TestKnownEntryPointStillSucceeds(t *testing.T) {
	ctx := context.Background()
	wasmPath := buildFixtureWasm(t, "basic")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

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

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-known-entry"))

	// place_order is testdata/basic's own entry point (PlaceOrder, snake_cased
	// by codegen), with the input engine/idempotency_test.go uses for it.
	const entry = "place_order"
	input := json.RawMessage(`{"userID":"user-1","cart":[{"sku":"ABC-123","quantity":1}]}`)

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, entry, input)
	if execErr != nil {
		if strings.Contains(execErr.Error(), "unknown entry point") {
			t.Fatalf("the guest rejected its OWN entry point %q: %v\n\n"+
				"Reporting unknown entry points as failures has turned every "+
				"dispatch into one, which is the way this change most likely "+
				"breaks.", entry, execErr)
		}
		t.Fatalf("entry point %q failed for another reason: %v\n\n"+
			"If testdata/basic was renamed, fix the name here -- the control is "+
			"meaningless against an entry point that does not exist.", entry, execErr)
	}
}
