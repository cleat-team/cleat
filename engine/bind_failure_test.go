package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestBindFailureIsAFailureNotAResult is the sibling of
// TestGuestReturnedErrorIsAFailureNotAResult (IMPROVEMENT-PLAN 3.22) for the
// one error exit that neither 3.22 nor 3.70 reached.
//
// cleatDispatch binds each entry-point parameter out of the payload before
// calling the workflow. When a bind fails it used to `return []byte("{...}")`
// -- a non-nil result -- and the generated main() re-reports whatever
// cleatDispatch hands back with cleat_complete(0, ...). So the host was told
// the workflow SUCCEEDED, with the bind error sitting in the result column as
// though it were the workflow's answer.
//
// That is the same shape as 3.22 (a Go workflow returning an error, reported
// as a success) and 3.70 (an unrecognised entry point, reported as a success).
// Both fixes are visible from the defect: 3.22's encodeJSONString was applied
// to this branch's message, and 3.70 gave the unknown-entry-point case a nil
// sentinel. This branch got the escaping without the status bit.
//
// Falsification: remove the cleatCompleteImport(1, ...) call from the bind
// branch in wasm/exports.go and the malformed case fails here with a nil error
// and `{"error":"unmarshal iterations: ..."}` as the result. Measured
// 2026-09-08 -- that is the pre-fix reading, and it is what this test exists to
// keep from coming back.
func TestBindFailureIsAFailureNotAResult(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Deliberately not a t.Skip, for the same reason as
		// guest_failure_test.go: the defect is specific to the Go-on-wasmtime
		// path, so skipping would report green having never run the branch.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	eng := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))

	// long_running takes `iterations int`. A string is a bind failure that
	// happens BEFORE the workflow body runs -- which is what makes it the
	// case 3.22's fix could not reach: there is no returned error to prefer,
	// and no panic to recover, because the entry point was never called.
	t.Run("a malformed parameter is an error", func(t *testing.T) {
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"long_running", []byte(`{"iterations":"not-a-number"}`))
		if err == nil {
			t.Fatalf("Execute returned no error for a payload that could not be bound; result = %q.\n\n"+
				"The bind failure was reported to the host with cleat_complete(0, ...) -- "+
				"status 0, success -- so the worker takes the success path and stores "+
				"status='done' with the error text in the result column. See 3.22.", res)
		}

		// The TYPE, not just the text. GuestReturnedError is what tells the
		// executor the guest stopped cleanly and said it had failed, rather
		// than trapping (3.23). A bind failure that arrives as a bare error is
		// labelled a trap, which is a different and wrong diagnosis.
		var gre *GuestReturnedError
		if !errors.As(err, &gre) {
			t.Errorf("error is %T, want *GuestReturnedError: a bind failure is a clean "+
				"guest-reported failure, not a trap (3.23). Got: %v", err, err)
		}

		// Name the parameter. A bind failure that does not say which field
		// failed leaves the caller to guess across every parameter.
		if !strings.Contains(err.Error(), "iterations") {
			t.Errorf("error does not name the parameter that failed to bind: %v", err)
		}
	})

	// The OTHER arm of the binding switch. cleat#1057 split it in two --
	// "int", "int64", "int32" apart from everything else -- and a fix applied
	// to one arm alone leaves the other silently reporting success. That
	// happened during this change: rebasing onto #1057 put the fix on the
	// complex-type arm only, and the int subtest above went red. Both arms are
	// asserted here so the next split cannot pass with half a fix.
	//
	// place_order takes `cart []CartItem`, a slice, which is the default arm.
	t.Run("a malformed complex parameter is an error", func(t *testing.T) {
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"place_order", []byte(`{"userID":"u","cart":"not-a-list"}`))
		if err == nil {
			t.Fatalf("Execute returned no error for an unbindable slice; result = %q", res)
		}
		var gre *GuestReturnedError
		if !errors.As(err, &gre) {
			t.Errorf("error is %T, want *GuestReturnedError: %v", err, err)
		}
		if !strings.Contains(err.Error(), "cart") {
			t.Errorf("error does not name the parameter that failed to bind: %v", err)
		}
	})

	// The control. Without it this test passes against a build where EVERY
	// call fails -- which is the failure mode a one-sided error assertion
	// cannot see.
	t.Run("a well-formed parameter still binds", func(t *testing.T) {
		res, _, _, _, _, err := eng.Execute(ctx, wasmBytes,
			"long_running", []byte(`{"iterations":1}`))
		if err != nil {
			t.Fatalf("a well-formed payload failed: %v", err)
		}
		if res != "done" {
			t.Errorf("result = %q, want \"done\": the parameter bound but the workflow "+
				"did not run to completion", res)
		}
	})
}
