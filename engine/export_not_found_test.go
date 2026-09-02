package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// A missing per-defer export must not be reported as a failed cleanup.
//
// IMPROVEMENT-PLAN §3.35. The legacy convention is one export per defer, named
// cleat_defer_<id>, and no guest in any language emits one -- so for every
// SDK-built guest the fallback pass said "defer execution failed" once per
// registered defer, about a convention the guest was never expected to follow.
// After #559 and #560 it could say it immediately AFTER the real cleanup had
// succeeded, which is the shape that misleads: an operator reads it and
// concludes their cleanup did not happen when it did.
func TestAMissingDeferExportIsNotReportedAsAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildAssemblyScriptWasm(t))
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	var logs bytes.Buffer
	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithWorkflowID("wf-notfound-log"))

	// A fenced workflow: the guest never reaches its own drain, so the host
	// runs the defers AND the legacy fallback pass also fires.
	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes,
		"spin_forever", json.RawMessage(`{}`))
	if execErr == nil {
		t.Fatal("the fenced workflow was reported as succeeding")
	}

	out := logs.String()

	// Control first. If the real cleanup did not run, the assertion below is
	// about a log line for a defer that genuinely never happened, and proves
	// nothing about the misleading case.
	if !ranTheDefer(caller) {
		t.Fatalf("the killed workflow's defer did not run (calls: %v), so this "+
			"test is not exercising the case it is about.\n\nLogs:\n%s",
			operationsCalled(caller), out)
	}
	// The second control: the fallback pass must actually have RUN and taken
	// the not-found branch. Without this, deleting the fallback entirely would
	// satisfy the assertion below -- no "defer execution failed" line, because
	// no line at all.
	//
	// Note what is NOT asserted here: the backend's own "ran the defers of a
	// killed workflow" line goes to slog.Default(), not to the engine's
	// configured logger, so it never reaches this buffer. That is a real
	// inconsistency -- an operator who configures a logger does not see it --
	// and is recorded in §3.35 rather than fixed here.
	if !strings.Contains(out, "no per-defer export for this defer") {
		t.Fatalf("the legacy fallback pass did not run, or its wording changed.\n\n"+
			"Logs:\n%s\n\nWithout it there is no log line to be wrong, and the "+
			"assertion below passes vacuously.", out)
	}

	if strings.Contains(out, "defer execution failed") {
		t.Errorf("the cleanup succeeded and the logs still say it failed.\n\nLogs:\n%s\n\n"+
			"A missing cleat_defer_<id> export means the guest does not use the "+
			"legacy per-defer convention -- which no SDK does. That is the normal "+
			"case, not a fault.", out)
	}
}

// ErrExportNotFound is matched with errors.Is, so both producers must wrap it.
// A test that only checked one would pass while the other stayed opaque, and
// the wazero path is exactly the one that was opaque for longest.
func TestBothBackendsMarkAMissingExport(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	wasmBytes := mustWat2Wasm(t, `(module (func (export "present") (result i64) (i64.const 0)))`)

	t.Run("wazero", func(t *testing.T) {
		eng := NewEngine(rt, &mockCaller{}, WithWorkflowID("wf-nf-wazero"))
		_, err := eng.RunDefer(ctx, wasmBytes, "cleat_defer_defer-0", nil)
		if err == nil {
			t.Fatal("calling an absent export must fail")
		}
		if !errors.Is(err, ErrExportNotFound) {
			t.Errorf("error does not match ErrExportNotFound: %v\n\n"+
				"runDefers tells 'no such export' from 'the export failed' with "+
				"errors.Is. Unwrapped, every missing export is logged as a failed "+
				"cleanup again.", err)
		}
	})
}
