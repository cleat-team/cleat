//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A workflow the Go runtime killed was recorded as having succeeded.
//
// IMPROVEMENT-PLAN 3.71. The Go-on-wasmtime path deliberately ignores the error
// from _start, because proc_exit is how every healthy Go guest leaves: main()
// returns and the wasip1 runtime exits, which surfaces as a non-nil error even
// on success. Before returning `"ok"` it checked for the two resource-limit
// trap codes.
//
// That check does not cover the case that matters. When the Go runtime cannot
// grow the heap past the configured memory limit it does not trap at all -- it
// prints a goroutine dump and calls proc_exit(2) from its fatal path. Not a
// *wasmtime.Trap, so resourceLimitError returns nil, so the guest fell through
// to Result: `"ok"` with a nil error.
//
// The consequence is the worst kind: the worker stores status='done' for a
// workflow that died partway through. Every step after the allocation never
// happened, there is no error text anywhere, and nothing retries it. This is
// 3.22's shape -- a failing guest handed back as a success -- on a different
// exit path, and worse, because 3.22 at least left the error text sitting in
// the result column.

// TestAGuestKilledByTheMemoryLimitIsNotReportedAsSuccess is the regression test.
//
// It asserts on Engine.Execute rather than the backend, because the defect is
// only visible where the two are composed: the backend returning `"ok"` is what
// the engine, and then the worker, believe.
func TestAGuestKilledByTheMemoryLimitIsNotReportedAsSuccess(t *testing.T) {
	ctx := context.Background()
	wasmBytes := fenceReentryWasm(t)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// 64 MB is comfortably above what the guest needs to start and comfortably
	// below what allocate_forever asks for, so the limit is what stops it.
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeMemoryLimits(64<<20, -1, -1))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-killed-by-memory-limit"))

	result, _, suspended, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"allocate_forever", json.RawMessage(`{}`))

	if execErr == nil {
		t.Fatalf("a workflow killed by the memory limit was reported as SUCCESS "+
			"(result=%q suspended=%v).\n\n"+
			"The worker stores status='done' for this. The workflow died partway "+
			"through, every step after the allocation never ran, and there is no "+
			"error text anywhere to find it by.", result, suspended != nil)
	}
	if result != "" {
		t.Errorf("execution failed but still returned a result %q; a killed guest "+
			"has no result to report", result)
	}

	// Name the mechanism. Any error would satisfy the check above, including the
	// fixture failing to build or the entry point being misspelled -- the
	// failure mode 2.10 is about, where a test passes because something
	// unrelated went wrong first.
	if !strings.Contains(execErr.Error(), "exited with status") {
		t.Errorf("the workflow failed, but not by being killed: %v\n\n"+
			"Expected the guest-exited-with-status path. A different error means "+
			"this test is no longer exercising the memory limit.", execErr)
	}
}

// TestAGuestRefusedByTheWasiPolicyIsNotReportedAsSuccess is the second
// instance of 3.71's hole, reached by a different cause: a genuine WASM TRAP
// that is neither a resource-limit trap nor a WASI exit at all.
//
// Demonstrated 2026-09-28 building the integration-hub tenant-sandbox wedge
// scenario (cleat#2597): a tenant-uploaded step whose filesystem read the
// host correctly refused (engine/wasi_policy.go's path_open, wasiFatal) was
// reported to its caller as status=done, result="ok" through a real deployed
// worker. The sandbox held; the WORKFLOW lied about it. Same shape as the
// test above, and it is why that fix widened ExitStatus's `ok` return rather
// than only its `code`: `ok == false` is every trap that is not a WASI exit,
// which covers this cause and the OOM cause with one test each rather than
// enumerating every way a guest can trap.
func TestAGuestRefusedByTheWasiPolicyIsNotReportedAsSuccess(t *testing.T) {
	ctx := context.Background()
	wasmBytes := fenceReentryWasm(t)

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
		WithWorkflowID("wf-refused-by-wasi-policy"))

	result, _, suspended, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"read_host_file", json.RawMessage(`{}`))

	if execErr == nil {
		t.Fatalf("a workflow the WASI policy refused was reported as SUCCESS "+
			"(result=%q suspended=%v).\n\n"+
			"The worker stores status='done' for this. The sandbox correctly "+
			"refused the guest's filesystem read; the workflow result did not "+
			"say so.", result, suspended != nil)
	}
	if result != "" {
		t.Errorf("execution failed but still returned a result %q; a trapped "+
			"guest has no result to report", result)
	}

	// Name the mechanism, for the same reason the test above does: any error
	// satisfies the check above, including an unrelated one.
	if !strings.Contains(execErr.Error(), "trapped and never reported a result") ||
		!strings.Contains(execErr.Error(), `cleat refuses the WASI call "path_open"`) {
		t.Errorf("the workflow failed, but not by the WASI policy refusing "+
			"path_open: %v\n\n"+
			"A different error means this test is no longer exercising the "+
			"policy trap.", execErr)
	}
}

// TestAHealthyGuestStillSucceeds is the control: the fix must not turn working
// workflows into failures.
//
// Read what it does and does not establish, because the obvious reading is
// wrong and I made it first. It does NOT prove the fix distinguishes exit 0
// from a non-zero exit, and it cannot: a healthy guest reports through
// cleat_complete, so Execute returns at the `completeResult != ""` branch and
// never reaches the startErr block this change touches. Measured by probe on
// 2026-09-02 -- an fmt.Fprintf at the top of that block printed nothing for
// this test.
//
// Which means the mutation that would prove it -- ignoring ExitStatus and
// treating any startErr as failure -- does not fail any test in this file. The
// `code != 0` guard is therefore conservative by construction rather than
// test-driven: it is written to leave the exit-0 path behaving EXACTLY as
// before, because that path is the historical reason startErr was ignored here
// at all and nothing measured says what still relies on it.
//
// What this control does establish is worth having on its own: a real Go guest
// still executes, reaches the host and is reported as successful through the
// composed Engine.Execute path, which is the thing a careless fix here breaks.
func TestAHealthyGuestStillSucceeds(t *testing.T) {
	ctx := context.Background()
	wasmBytes := fenceReentryWasm(t)

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

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-healthy"))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"after_the_fence", json.RawMessage(`{}`))
	if execErr != nil {
		t.Fatalf("a healthy workflow was reported as failed: %v\n\n"+
			"Every Go guest leaves through proc_exit, so a fix that keys on "+
			"\"_start returned an error\" rather than on a NON-ZERO exit status "+
			"fails all of them.", execErr)
	}
	// And it has to have actually run, or "no error" is meaningless.
	found := false
	for _, rec := range caller.calls {
		if rec.Op == "after_the_fence" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the healthy workflow reported success without reaching the host "+
			"(calls: %v), so this control passes for a guest that did nothing",
			operationsCalled(caller))
	}
}
