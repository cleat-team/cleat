//go:build cgo

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// A defer body ran twice after a trap.
//
// executeCompiled's non-suspend-error branch called invokeDefersOnTrap (the
// still-live module) and then runDefers (a fresh module), unconditionally,
// under a comment saying "Try running defers on the still-live module first,
// then fall back to fresh-module defers". There was no conditional. Measured
// 2026-09-01 with the guest below -- one registered defer, entry point traps,
// defer body traps -- the two paths logged two failures for the SAME defer,
// each carrying the trap raised by the defer function itself, so both had
// reached the body:
//
//	"defer execution failed" defer_id=defer-0 description=cleanup error="wasm trap: unreachable ... .$2"
//	"defer execution failed" workflow_id=wf... defer_id=defer-0 export=cleat_defer_defer-0 error="wasm trap: unreachable ... .$2"
//
// A defer is a destructor, so a doubled body is a doubled effect: a
// compensating saga step applied twice, a lock released twice, a notification
// sent twice.
//
// Scope, stated so nobody has to re-derive it: this is NOT the worker.
// executeCompiled is reached only when no backend is registered -- cleatctl
// replay|debug, cleat run, cleat-bench, and the public testing packages
// cleat/wasmtest, cleat/cleattest, cleat/embedded. That last group is why it
// matters rather than why it does not: a user testing a compensating defer saw
// it fire twice under the harness and once in production, so the harness
// disagreed with the runtime in the direction that makes a real
// double-compensation look like a test artifact.
//
// IMPROVEMENT-PLAN 3.35 finding 3.

// twoDefersOneExportedWat registers two defers and then traps.
//
// Only the FIRST has a cleat_defer_* export, which is what lets one guest carry
// both halves of the property:
//
//	defer-0  export present  -> invoked on the live module, must NOT be re-run
//	defer-1  export absent   -> not invocable there, must still reach the fall-back
//
// Without the second, "run each defer once" would be satisfied by deleting the
// fall-back entirely, and a defer the live module could not offer would simply
// be dropped.
//
// Both the entry point and the defer body trap. The trapping body is what makes
// execution observable: a defer that merely returned would log nothing, and the
// count below would be zero either way.
const twoDefersOneExportedWat = `(module
  (import "env" "cleat_defer" (func $defer (param i32 i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 1024) "cleanup-a")
  (data (i32.const 1088) "cleanup-b")
  (func (export "run") (param i32 i32 i32 i32) (result i64)
    (drop (call $defer (i32.const 1024) (i32.const 9) (i32.const 2048) (i32.const 64)))
    (drop (call $defer (i32.const 1088) (i32.const 9) (i32.const 3072) (i32.const 64)))
    unreachable)
  (func (export "cleat_defer_defer-0") (param i32 i32 i32 i32) (result i64) unreachable)
)`

// deferLogRE matches one "defer execution failed" record and captures the
// defer_id it names, which is the only field both call sites log.
var deferLogRE = regexp.MustCompile(`msg="defer execution failed"[^\n]*?defer_id=(\S+)`)

// deferNotFoundRE matches invokeDefersOnTrap's other outcome.
var deferNotFoundRE = regexp.MustCompile(`msg="defer export not found"[^\n]*?defer_id=(\S+)`)

func countByDeferID(re *regexp.Regexp, logs string) map[string]int {
	out := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(logs, -1) {
		out[m[1]]++
	}
	return out
}

// runTwoDefersAndTrap runs the guest above through the wazero path -- an Engine
// with a Runtime and no backends, which is exactly how cleatctl replay, cleat
// run, cleat-bench and cleat/wasmtest build theirs -- and returns what it
// logged.
func runTwoDefersAndTrap(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	var logs bytes.Buffer
	eng := NewEngine(rt, &mockCaller{},
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithWorkflowID("wf-defer-once"))
	if len(eng.backends) != 0 {
		t.Fatalf("expected an engine with no backends, got %d.\n\n"+
			"With one registered, Execute routes to executeWithBackend and never "+
			"reaches the branch under test -- this would pass having exercised "+
			"nothing.", len(eng.backends))
	}

	_, _, _, _, _, execErr := eng.Execute(ctx, mustWat2Wasm(t, twoDefersOneExportedWat),
		"run", json.RawMessage(`{}`))
	if execErr == nil {
		t.Fatal("the guest's entry point traps; Execute must report that.\n\n" +
			"Without a failure there is no non-suspend-error branch and no defers run.")
	}
	return logs.String()
}

// TestDeferBodyRunsOnceAfterATrap is the regression test and its control, in
// one run, because they are two facts about the same execution.
func TestDeferBodyRunsOnceAfterATrap(t *testing.T) {
	logs := runTwoDefersAndTrap(t)

	failures := countByDeferID(deferLogRE, logs)
	notFound := countByDeferID(deferNotFoundRE, logs)

	// Vacuous-pass control. If the guest stopped registering defers, or the log
	// wording changed, every assertion below would hold over an empty map.
	if len(failures) == 0 {
		t.Fatalf("no \"defer execution failed\" records at all.\n\nLogs:\n%s\n\n"+
			"Either no defer ran -- which is a worse bug than the one this test is "+
			"about -- or deferLogRE stopped matching and this test now checks nothing.",
			logs)
	}

	// The regression: the defer the live module could run must run once.
	if n := failures["defer-0"]; n != 1 {
		t.Errorf("defer-0's body executed %d times, want 1.\n\nLogs:\n%s\n\n"+
			"invokeDefersOnTrap ran it on the live module and runDefers ran it again "+
			"on a fresh one. A defer is a destructor: a doubled body is a doubled "+
			"effect -- a compensating step applied twice, a lock released twice.",
			n, logs)
	}

	// The control: the defer the live module could NOT offer must still reach
	// the fall-back. Without this, deleting the fall-back passes the assertion
	// above.
	if notFound["defer-1"] != 1 {
		t.Errorf("defer-1 was not reported missing from the live module (%d records).\n\nLogs:\n%s\n\n"+
			"The guest exports cleat_defer_defer-0 and not cleat_defer_defer-1, so "+
			"invokeDefersOnTrap must say so and hand it on.", notFound["defer-1"], logs)
	}
	if failures["defer-1"] != 1 {
		t.Errorf("defer-1 reached the fresh-module fall-back %d times, want 1.\n\nLogs:\n%s\n\n"+
			"A defer the live module could not offer must still be attempted, or the "+
			"fix has turned a doubled defer into a dropped one.", failures["defer-1"], logs)
	}
}

// TestInvokeDefersOnTrapReportsWhatItCouldNotRun pins the contract the fix rests
// on, at the unit level: "invoked" means the export was found and called,
// whatever the outcome.
//
// A defer that RAN AND TRAPPED must not be reported as un-invoked, or the
// caller retries it on a fresh instance and the doubling comes straight back --
// in the case where it is most harmful, since a defer that trapped may have
// already applied part of its effect.
func TestInvokeDefersOnTrapReportsWhatItCouldNotRun(t *testing.T) {
	ctx := context.Background()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	var logs bytes.Buffer
	eng := NewEngine(rt, &mockCaller{},
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))

	compiled, err := rt.CompileModule(ctx, mustWat2Wasm(t, twoDefersOneExportedWat))
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)
	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	notInvoked := eng.invokeDefersOnTrap(ctx, mod, map[string]string{
		"defer-0": "exported, body traps",
		"defer-1": "not exported",
	})

	if _, ok := notInvoked["defer-0"]; ok {
		t.Errorf("defer-0 was reported as not invoked, but its export exists and was "+
			"called -- it trapped.\n\nnotInvoked = %v\n\n"+
			"Reporting a trapped defer as un-invoked makes the caller retry it on a "+
			"fresh instance, which is the doubling this returns a value to prevent, in "+
			"the case where it is most harmful: a defer that trapped may already have "+
			"applied part of its effect.", notInvoked)
	}
	if _, ok := notInvoked["defer-1"]; !ok {
		t.Errorf("defer-1 has no cleat_defer_defer-1 export and was not reported as "+
			"un-invoked.\n\nnotInvoked = %v\n\nIt would be silently dropped.", notInvoked)
	}
	if len(notInvoked) != 1 {
		t.Errorf("notInvoked = %v, want exactly defer-1", notInvoked)
	}
	if !strings.Contains(logs.String(), "defer execution failed") {
		t.Errorf("invokeDefersOnTrap ran a trapping defer without logging the failure.\n\n"+
			"Logs:\n%s", logs.String())
	}
}
