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

// IMPROVEMENT-PLAN 3.23: a guest that returned an error was reported as a trap.
//
// resolveWasmTrap prefixes "wasm trap: " onto any non-empty message, so a
// workflow that simply returned an error produced:
//
//	host: workflow <id>: execution failed: wasm trap: host: export "x" failed: <their error>
//
// A claim of a memory fault over the author's own error text. The guest stopped
// cleanly and *said* it had failed; the label sends the reader looking at the
// runtime instead of at their workflow.
//
// This was latent until 3.22. Before it a guest-returned error never reached
// this path -- it was reported as a success -- so everything arriving here
// genuinely was a trap and the unconditional prefix was right. 3.22 introduced
// a second class of error to a function that labels all of them the same.
//
// The two tests below are a pair and only mean something together: one asserts
// the label is gone where it was wrong, the other that it survives where it is
// right. Without the second, deleting the label unconditionally would pass.

// TestGuestReturnedErrorIsNotLabelledATrap is the regression test.
//
// basic.PlaceOrder returns fmt.Errorf("cart is empty") on an empty cart, before
// it reaches any durable call. No service, no database, no crash -- just a
// workflow that returned an error.
func TestGuestReturnedErrorIsNotLabelledATrap(t *testing.T) {
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

	eng := NewEngine(rt, &mockCaller{}, withWasmtimeBackend(t))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "place_order",
		json.RawMessage(`{"userID":"test-user","cart":[]}`))
	if execErr == nil {
		t.Fatal("workflow returned an error but Execute did not (see 3.22)")
	}

	msg := execErr.Error()
	if strings.Contains(msg, "wasm trap") {
		t.Errorf("a guest-returned error is labelled a trap:\n  %s\n\n"+
			"Nothing faulted. The workflow ran correctly and reported a failure, "+
			"and this message sends the reader looking for a memory fault.", msg)
	}
	// The author's own text is the part that should survive, and the reason the
	// label matters: it is what the reader needs to end up looking at.
	if !strings.Contains(msg, "cart is empty") {
		t.Errorf("the workflow's own error text is missing:\n  %s", msg)
	}

	var guestErr *GuestReturnedError
	if !errors.As(execErr, &guestErr) {
		t.Errorf("error does not carry *GuestReturnedError, so nothing downstream "+
			"can tell it from a trap either:\n  %T: %s", execErr, msg)
	}
}

// TestRealTrapIsStillLabelledATrap is the control.
//
// It is the half that makes the test above mean something: the cheapest way to
// pass that one is to stop labelling anything, and this fails if you do. A
// guest spinning past its execution limit is interrupted by wasmtime's epoch
// mechanism, which is a genuine trap and must still say so.
func TestRealTrapIsStillLabelledATrap(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "spin"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Same shape as TestIntegrationWorkflowMaxDuration: a 2s budget so
	// instantiation is a small fraction of it, and an iteration count no
	// machine finishes.
	const limit = 2 * time.Second
	const iterations = 100000000000

	eng := NewEngine(rt, &mockCaller{}, withWasmtimeBackend(t), WithDefaultWorkflowTimeout(limit))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "spin",
		json.RawMessage(fmt.Sprintf(`{"iterations":%d}`, iterations)))
	if execErr == nil {
		t.Fatal("expected the execution-time limit to fire")
	}

	if !strings.Contains(execErr.Error(), "wasm trap") {
		t.Errorf("a genuine trap lost its label:\n  %s\n\n"+
			"3.23 narrowed the label to non-guest errors; it must not have "+
			"removed it. This is a real epoch interruption.", execErr.Error())
	}

	// The substring above is NOT sufficient on its own, and assuming it was is
	// how this test first passed against a deliberately broken build. The
	// backend's own message already contains "wasm trap", so the text survives
	// even if the executor stops applying the trap envelope entirely --
	// measured 2026-08-31 by disabling the resolveWasmTrap branch, which this
	// test passed. What actually distinguishes the branches is the type.
	var trapErr *wasmTrapError
	if !errors.As(execErr, &trapErr) {
		t.Errorf("a genuine trap did not go through the trap envelope:\n  %T: %s\n\n"+
			"The DWARF-enriched message is built there, so losing this branch "+
			"loses the source locations even when the text still says 'trap'.",
			execErr, execErr.Error())
	}

	var guestErr *GuestReturnedError
	if errors.As(execErr, &guestErr) {
		t.Errorf("a trap was marked as a guest-returned error: %s", execErr.Error())
	}
}

// TestGuestReturnedErrorPreservesTheChain guards the wrapper itself. It carries
// no message of its own -- the backend has already built one -- so wrapping
// must not alter the text or break errors.Is.
func TestGuestReturnedErrorPreservesTheChain(t *testing.T) {
	base := errors.New(`host: export "place_order" failed: cart is empty`)
	wrapped := &GuestReturnedError{Err: base}

	if wrapped.Error() != base.Error() {
		t.Errorf("message changed by the marker:\n  got  %s\n  want %s", wrapped.Error(), base.Error())
	}
	if !errors.Is(wrapped, base) {
		t.Error("marker broke the error chain")
	}

	var got *GuestReturnedError
	if !errors.As(fmt.Errorf("outer: %w", wrapped), &got) {
		t.Error("errors.As cannot find the marker through an outer wrap, which is " +
			"how the executor looks for it")
	}
}
