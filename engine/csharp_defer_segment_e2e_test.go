//go:build cgo

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// A C# guest earns its place in deferSegmentLanguages the way every other language did: a real guest crosses the boundary.
// testdata/csharp_defer_order holds a real C# component (checked in, so no .NET toolchain is needed) that registers a
// defer, makes one call of the body's own, and drains the defers when the body completes.
//
// Run as a defer segment the body's call must be REFUSED and only the defer may reach the ServiceCaller; run as an
// ordinary execution of the SAME entry point the body's call happens too. The pair is what separates "the stop is
// conditional on the segment" from "this guest does not call anything": a segment that records only [Refund] would
// otherwise also be what a guest that never makes the body call records.
//
// The guest is a spike and not an SDK. The C# SDK has to pass this test unchanged.

func csharpDeferSegment(t *testing.T, wfID string, deferPhase bool) (string, *SuspendResult, []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "csharp_defer_order", "defer_order.wasm"))
	if err != nil {
		t.Fatalf("read the C# fixture: %v", err)
	}
	// Stamped as the C# build chain will stamp it. Unstamped, a component is detected as python.
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the fixture declares %q, not csharp, so deferSegmentLanguages is not what is being tested", got)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	// "csharp" is added to the backend list here until the label lands in WasmtimeLanguages.
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	opts := []EngineOption{WithBackends(langs, wt), WithWorkflowID(wfID)}
	if deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	eng := NewEngine(rt, caller, opts...)
	res, _, suspended, _, _, err := eng.Execute(ctx, b, "run", []byte(`{"mode":"defer_order"}`))
	if err != nil {
		t.Fatalf("execute (deferPhase=%v) failed: %v", deferPhase, err)
	}
	return res, suspended, operationsCalled(caller)
}

func TestCSharpDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	res, susp, got := csharpDeferSegment(t, "wf-csharp-defer-segment", true)
	if susp == nil {
		t.Fatalf("the C# defer segment did not suspend; it returned %q.\n\nOperations recorded: %v", res, got)
	}
	if len(got) != 1 || got[0] != "Refund" {
		t.Fatalf("the C# defer segment recorded %v, want exactly [Refund]: the body's own call must be refused", got)
	}
}

// The control for the test above: the same fixture and entry point with no defer phase.
func TestCSharpOrdinarySegmentRunsTheBody(t *testing.T) {
	res, susp, got := csharpDeferSegment(t, "wf-csharp-ordinary", false)
	if susp != nil {
		t.Fatalf("the ordinary C# execution suspended: %v", susp.Reason)
	}
	if len(got) != 2 || got[0] != "Body" || got[1] != "Refund" {
		t.Fatalf("the ordinary C# execution recorded %v, want exactly [Body Refund] (result %q)", got, res)
	}
}
