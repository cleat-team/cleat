//go:build cgo

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// The host kept trying to run defers the guest had already run.
//
// Since the guest runs its own defer bodies when its entry point finishes
// (IMPROVEMENT-PLAN 3.70), the host-side pass over the same deferrals has
// nothing left to do. It still ran, looked for an export named
// "cleat_defer_<id>" that no guest has ever had, and logged
//
//	defer execution failed ... error="... unknown entry point: cleat_defer_defer-0"
//
// for a defer that had in fact just run. That is worse than the silence it
// replaced: an operator reading it concludes their cleanup did not happen and
// goes looking for why, and the log is the only evidence they have.
//
// The rule is now: the host runs defers only for a guest that never got the
// chance to run its own. A guest that reached cleat_complete -- with a result
// or with an error -- ran them on the way out. A guest that trapped, was
// fenced, or timed out did not, and there the host attempt is the only chance
// and its failure log is a true statement.
//
// That covers every SDK, not just Go. The other four have no defer bodies at
// all, so for them "the host has nothing to add" holds for a different reason.

// captureLogs returns a logger writing into buf, at a level that includes the
// WARN the defer pass emits.
func captureLogs(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestHostDoesNotRerunDefersAGuestAlreadyRan is the regression test.
//
// It uses the error path deliberately. A workflow that fails is the case a
// defer exists for, and it is also the path that reaches the host's defer pass
// -- a successful Execute never enters that branch at all, so the success-side
// duplicate lived in the worker (deleted with this change) rather than here.
func TestHostDoesNotRerunDefersAGuestAlreadyRan(t *testing.T) {
	ctx := context.Background()
	wasmPath := buildFixtureWasm(t, "deferfunc")
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

	var logs bytes.Buffer
	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-no-rerun"),
		WithLogger(captureLogs(&logs)))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"defer_on_error", json.RawMessage(`{}`))
	if execErr == nil {
		t.Fatal("defer_on_error was expected to fail; if the fixture changed " +
			"this test no longer reaches the host's defer pass")
	}

	// The defer ran, once, in the guest. This half is not decoration: the
	// cheapest way to stop the false log is to stop running defers entirely,
	// which would pass a test that only looked at the log.
	if got := operationsCalled(caller); len(got) != 1 || got[0] != "on_error" {
		t.Fatalf("recorded %v, want exactly [on_error] -- the guest's own defer, "+
			"run once", got)
	}

	if strings.Contains(logs.String(), "defer execution failed") {
		t.Errorf("the host reported a defer failure for a defer that ran.\n\nLogs:\n%s\n\n"+
			"The guest runs its own defer bodies now. The host's pass finds no "+
			"cleat_defer_<id> export -- no guest has ever had one -- and logs the "+
			"miss as a failure, telling an operator their cleanup did not happen "+
			"when it did.", logs.String())
	}
}

// TestHostStillRunsDefersWhenTheGuestTrapped is the control, and it guards the
// boundary the fix rests on.
//
// "Stop re-running defers" must not become "stop running defers". A guest that
// trapped never reached its own defer runner, so the host's pass is the only
// thing left -- and when it cannot find the export, saying so is correct. If
// this test and the one above cannot both hold, the rule is wrong.
//
// The module traps rather than completing, and exports one of the two defers,
// so the pass has both something to run and something to report.
func TestHostStillRunsDefersWhenTheGuestTrapped(t *testing.T) {
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
	defer wt.Close(ctx)

	// No cleat.metadata, so DetectLanguage classifies it "go" and it routes to
	// wasmtime; no _start, so Execute takes the direct-export branch. The entry
	// point registers a defer with the host and then traps, which is what makes
	// this the "guest never ran its own" case.
	const trappingWat = `(module
	  (import "env" "cleat_defer" (func $defer (param i32 i32 i32 i32) (result i64)))
	  (memory (export "memory") 1)
	  (data (i32.const 1024) "cleanup")
	  (func (export "run") (param i32 i32 i32 i32) (result i64)
	    (drop (call $defer (i32.const 1024) (i32.const 7) (i32.const 2048) (i32.const 64)))
	    unreachable)
	  (func (export "cleat_defer_defer-0") (param i32 i32 i32 i32) (result i64)
	    unreachable))`

	var logs bytes.Buffer
	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-trapped"),
		WithLogger(captureLogs(&logs)))

	_, _, _, _, _, execErr := eng.Execute(ctx, mustWat2Wasm(t, trappingWat),
		"run", json.RawMessage(`{}`))
	if execErr == nil {
		t.Fatal("the guest was expected to trap")
	}

	// The defer export traps too, so the host's attempt fails -- and that
	// failure is the evidence the attempt happened at all.
	if !strings.Contains(logs.String(), "defer execution failed") {
		t.Errorf("a guest that trapped had its defers skipped.\n\nLogs:\n%s\n\n"+
			"It never reached its own defer runner, so the host's pass is the only "+
			"chance the cleanup has. Suppressing it here would turn a fix for a "+
			"misleading log into a dropped defer.", logs.String())
	}
}

// TestAPanickingGuestRunsItsOwnDefers pins the boundary this fix draws, at the
// transition most likely to be assumed to fall on the other side of it.
//
// "Panic" reads like "trap", and if it were one the guest would have been
// stopped before its wrapper returned and its defers would be the host's
// problem. It is not. A Go panic unwinds into the generated dispatcher's
// recover, which reports the failure through cleat_complete, so the guest
// still leaves through its own wrapper -- and the wrapper runs the defer
// bodies before reporting.
//
// Measured 2026-09-02 on a real Go SDK guest through wasmtime: the cleanup
// call was recorded, the failure came back as a GuestReturnedError carrying the
// panic message, and the host logged nothing.
//
// This matters for scoping IMPROVEMENT-PLAN 3.35 phase 4, which is about
// workflows whose defers nothing runs. Panics are not in that set. What is
// left there is the genuinely unrecoverable: a WASM trap, an out-of-memory,
// and a fence kill or timeout -- none of which return to guest code at all.
func TestAPanickingGuestRunsItsOwnDefers(t *testing.T) {
	ctx := context.Background()
	wasmPath := buildFixtureWasm(t, "deferfunc")
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

	var logs bytes.Buffer
	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-panic"),
		WithLogger(captureLogs(&logs)))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"defer_on_panic", json.RawMessage(`{}`))
	if execErr == nil {
		t.Fatal("defer_on_panic was expected to fail")
	}
	if !strings.Contains(execErr.Error(), "the workflow panicked") {
		t.Fatalf("failed for a different reason than the fixture's own panic, so "+
			"the defer may not have been reached: %v", execErr)
	}

	if got := operationsCalled(caller); len(got) != 1 || got[0] != "on_panic" {
		t.Errorf("recorded %v, want exactly [on_panic].\n\n"+
			"A panic is recovered by the generated dispatcher, so the guest leaves "+
			"through its own wrapper and its defers run there.", got)
	}
	if strings.Contains(logs.String(), "defer execution failed") {
		t.Errorf("the host ran a defer pass for a guest that completed.\n\nLogs:\n%s\n\n"+
			"A recovered panic reaches cleat_complete, so it is a GuestReturnedError "+
			"and the pass must be skipped.", logs.String())
	}
}
