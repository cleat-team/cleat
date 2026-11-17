package typescript

// The end-to-end half of cleat#3826's second part, and what lets `javascript` into engine.deferSegmentLanguages.
//
// That list's own comment defines membership as "verified to unwind on the suspend sentinel ... not 'ought to'", and
// states the failure when it is wrong: a guest that reads the sentinel through an ordinary result layout sees
// responseLen=0, errCode=0 -- an EMPTY SUCCESSFUL RESPONSE -- carries on past the stop, does the new work the segment
// exists to prevent, and reports a terminated workflow as completed, with no error anywhere.
//
// BOTH ARMS ARE HERE, on the SAME entry point:
//   - the defer segment (WithDeferPhase) records only the defers, and suspends;
//   - the control without the defer phase records the BODY's call too.
// The control is what separates "the stop is conditional on the segment" from "this guest stopped working".

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// recordingCaller is the ServiceCaller the assertions read: it records "<service>/<operation>" in call order, so a
// body call that should have been refused is visible as an entry rather than as a silently different result.
type recordingCaller struct {
	mu  sync.Mutex
	ops []string
}

func (c *recordingCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, service+"/"+operation)
	return `{}`, nil
}

func (c *recordingCaller) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ops...)
}

// deferSegmentEngine builds an Engine wired to c, with the defer phase on when deferPhase is true.
func deferSegmentEngine(t *testing.T, c engine.ServiceCaller, deferPhase bool) *engine.Engine {
	t.Helper()
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	opts := []engine.EngineOption{
		engine.WithWorkflowID("wf-ts-defer-segment"),
		engine.WithDefName("defer_segment"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	}
	if deferPhase {
		opts = append(opts, engine.WithDeferPhase())
	}
	return engine.NewEngine(rt, c, opts...)
}

func buildDeferSegment(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/defer_segment.ts"
	res := build(t, root, src, "defer_segment")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasmBytes, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	// deferSegmentLanguages is keyed by what the guest declares, and DetectLanguage returns the module's own metadata
	// verbatim -- so this asserts the KEY the list is consulted with, not a guess at it.
	if lang := wasm.DetectLanguage(wasmBytes); lang != "javascript" {
		t.Fatalf("the built module declares language %q, not \"javascript\"", lang)
	}
	return wasmBytes
}

func TestTypeScriptDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	wasmBytes := buildDeferSegment(t)

	caller := &recordingCaller{}
	e := deferSegmentEngine(t, caller, true)

	_, _, susp, _, _, err := e.Execute(context.Background(), wasmBytes, "defer_segment", []byte(`{}`))
	if err != nil {
		t.Fatalf("the defer segment failed: %v\n\n"+
			"An error here usually means the language is not in deferSegmentLanguages: the engine fails the segment "+
			"closed rather than risk running the body past the stop.", err)
	}
	if susp == nil {
		t.Fatalf("the TypeScript defer segment did not suspend; it returned an outcome.\n\n"+
			"The segment must stop on the sentinel, not report a result: a returned result means the guest read the "+
			"stop as an ordinary value and carried on. Operations recorded: %v", caller.snapshot())
	}

	got := caller.snapshot()
	for _, op := range got {
		if op == "inventory/body" {
			t.Fatalf("the workflow BODY reached the ServiceCaller during a defer segment: %v.\n\n"+
				"This is the host half: the body's call must be refused past the frontier before the caller is "+
				"reached. Seeing it means the refusal is gone rather than that the guest decoded wrongly.", got)
		}
	}
	want := []string{"inventory/second", "inventory/first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the defer segment recorded %v, want exactly %v.\n\n"+
			"An empty list means the drain never ran; reversed order means it ran registration-order rather than "+
			"LIFO; a list of one means the drain stopped at the first body.", got, want)
	}
}

// The control: the SAME entry point with no defer phase. It must record the body's call and then the two defers, which
// is what makes the arm above a statement about the SEGMENT rather than about a guest that stopped calling out.
func TestTypeScriptDeferSegmentControlWithoutTheDeferPhaseRecordsTheBody(t *testing.T) {
	wasmBytes := buildDeferSegment(t)

	caller := &recordingCaller{}
	e := deferSegmentEngine(t, caller, false)

	_, _, susp, _, _, err := e.Execute(context.Background(), wasmBytes, "defer_segment", []byte(`{}`))
	if err != nil {
		t.Fatalf("running the workflow without a defer phase failed: %v", err)
	}
	if susp != nil {
		t.Fatalf("the workflow suspended without a defer phase (%s)", susp.Reason)
	}
	got := caller.snapshot()
	sawBody := false
	for _, op := range got {
		if op == "inventory/body" {
			sawBody = true
		}
	}
	if !sawBody {
		t.Fatalf("the control never recorded the body's call, so it cannot distinguish a working guest from a "+
			"stopped segment. Recorded: %v", got)
	}
}
