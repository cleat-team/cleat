//go:build cgo

package main

import (
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/engine"
)

// TestDefersRunOnTheFencedBackend records a second execution path that has no
// execution limit at all.
//
// A workflow's guest code runs on whichever backend WasmtimeLanguages routes it
// to, and is bounded there by epoch interruption. Its *deferred callbacks* are
// ordinary guest code too -- a defer body can loop exactly like a workflow body
// can -- but they never reach a backend. Engine.RunDefer does not consult
// backendForWasm: it reaches straight for e.rt, the wazero Runtime, and when
// that is nil, which is precisely the case when wasmtime is handling execution,
// it builds a fresh wazero Runtime for the defer.
//
// So every defer in cleat runs on wazero whatever the routing table says, and
// wazero only observes context cancellation when the guest calls back into the
// host (IMPROVEMENT-PLAN 2.28). A defer that loops without doing so is not
// stopped: it holds its worker slot until the process dies.
//
// Measured, not read. With this test unskipped, against a 1s wasmtime budget:
//
//	defer_backend_test.go: a deferred callback that never returns was still
//	running after 20s with a 1s execution budget
//
// Getting there took one correction worth keeping, because it is the failure
// mode this repo keeps hitting. The first version of this test declared the
// defer export with no parameters, and it *passed* -- in 0.01s, with the fix
// reverted, having never executed the guest at all. The export was rejected
// with "expected 0 params, but passed 4", and runDefers logs defer failures
// without propagating them, so a test that ran nothing was indistinguishable
// from a test that proved something. The signature below is the one CallExport
// actually calls.
//
// Skipped rather than left red, and rather than deleted, which is the treatment
// TestPythonWasmEndToEnd had for the same reason: a red develop helps nobody,
// and deleting this would lose the only thing that demonstrates the gap.
// Unskipping it is the acceptance test for 3.32.
//
// It is recorded rather than fixed because it is not a one-line change. Defers
// execute with no HostHandler in ctx today, so routing them to a backend
// changes what a defer body is allowed to do -- a defer that calls a service is
// plausible cleanup, and that question wants deciding rather than falling out
// of a routing change.
func TestDefersRunOnTheFencedBackend(t *testing.T) {
	t.Skip("known: defers always run on wazero, unfenced; see IMPROVEMENT-PLAN 3.32 -- unskipping this is the acceptance test")

	// A module exporting a defer body that spins. Built from WAT rather than a
	// checked-in fixture so it cannot drift, and so what it does is legible:
	// an empty infinite loop, no host calls, nothing for a
	// cancellation-at-the-host-boundary check to notice.
	//
	// The export name is the convention runDefers looks up ("cleat_defer_" +
	// the deferral's ID) and the signature is what CallExport passes.
	wasmBytes, err := wasmtime.Wat2Wasm(`(module
	  (func (export "cleat_defer_defer-1") (param i32 i32 i32 i32) (result i64)
	    (loop $forever
	      (br $forever))
	    (i64.const 0))
	  (memory (export "memory") 1))`)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	const budget = time.Second
	wt, err := engine.NewWasmtimeBackend(t.Context(),
		engine.WithWasmtimeExecutionTimeout(budget))
	if err != nil {
		// Not a skip. This file is built with cgo, so a wasmtime backend that
		// will not construct is a real failure, and skipping here would read as
		// "defers are fenced" while testing nothing.
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}

	w := newTestWorker(&mockStore{})
	memMB, instrLimit := 0, 0
	w.wasmMemoryMaxMB = &memMB
	w.wasmInstructionLimit = &instrLimit
	w.wasmtimeBackend = wt

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runDefers(wasmBytes, map[string]string{"defer-1": "spin"})
	}()

	// Bounded rather than left to hang the package, so a regression names the
	// defect instead of timing out the job -- the same shape 2.28's parked
	// wazero fence test was written with. Generous relative to the 1s budget:
	// the assertion is "something stopped it", and today nothing does, so any
	// finite wait separates the two.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("a deferred callback that never returns was still running after 20s "+
			"with a %s execution budget: defers do not reach a backend at all, so "+
			"the fence never applies to them", budget)
	}
}
