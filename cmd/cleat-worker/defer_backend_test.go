//go:build cgo

package main

import (
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/engine"
)

// TestDefersRunOnTheFencedBackend asserts that a deferred callback is subject
// to the same execution limit as the workflow that registered it.
//
// A defer body is ordinary guest code -- it can loop exactly like a workflow
// body can. Until 2026-08-05 it was not bounded at all: Engine.RunDefer never
// consulted backendForWasm, reaching straight for the wazero Runtime and, when
// that was nil (exactly the case when wasmtime is handling execution), building
// a fresh wazero one for the defer. So every defer in cleat ran on wazero
// whatever the routing table said, and wazero cannot be bounded for a guest
// that never calls into the host -- three mechanisms measured and rejected in
// IMPROVEMENT-PLAN 3.32. A defer that looped held its worker slot until the
// process died.
//
// This test was written while that was true, skipped, and recorded as 3.32's
// acceptance criterion. It is unskipped now. Against a 1s budget it went from
// still running after 20s to returning in 1.00s.
//
// Getting it right took one correction worth keeping, because it is the failure
// mode this repo keeps hitting. The first version declared the defer export
// with no parameters, and it *passed* -- in 0.01s, with the fix reverted,
// having never executed the guest at all. The export was rejected with
// "expected 0 params, but passed 4", and runDefers logs defer failures without
// propagating them, so a test that ran nothing was indistinguishable from one
// that proved something. The signature below is the one CallExport calls.
//
// What this does NOT assert: that a defer can make host calls. It still
// cannot, because defers run with no HostHandler in ctx -- which is a defect
// in the current implementation rather than a property of defers.
// IMPROVEMENT-PLAN 3.35 is the design that fixes it, by running defers in a
// replayed instance with a live session. This test is about the bound, and
// deliberately asserts nothing about the semantics that design will change.
func TestDefersRunOnTheFencedBackend(t *testing.T) {
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
