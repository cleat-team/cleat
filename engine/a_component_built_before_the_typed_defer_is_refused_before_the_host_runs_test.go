//go:build cgo

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#4079: durable-defer is result<string, call-failure> now. A component built against the old `-> string` signature is
// refused on its FIRST durable-defer call, BEFORE the host runs, so it records nothing.
//
// testdata/csharp_defer_order/defer_order_pre4079.wasm is the C# spike fixture as it was built against the old signature, kept as
// a real stale component. Measured here: with the refusal disabled the component dies with a generic wasm trap and not this
// message. The reason for refusing BEFORE the host runs is cleat#3786's measurement for the same mechanism: wasmtime checks a
// dynamically-typed host function against its caller only when it returns, after the host work has run, so a stale defer would
// otherwise have been recorded. The no-`defer`-event assertion below holds with the refusal and was not observed failing without it.
func TestAComponentBuiltBeforeTheTypedDeferIsRefusedBeforeTheHostRuns(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "csharp_defer_order", "defer_order_pre4079.wasm"))
	if err != nil {
		t.Fatalf("read the stale fixture: %v", err)
	}
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
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
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	eng := NewEngine(rt, caller, WithBackends(langs, wt), WithWorkflowID("wf-stale-defer"))
	_, history, _, _, _, err := eng.Execute(ctx, b, "run", []byte(`{"mode":"defer_order"}`))
	if err == nil {
		t.Fatal("a component built against the old durable-defer ran; it must be refused")
	}
	if !strings.Contains(err.Error(), "durable-defer import returned a bare string") {
		t.Fatalf("refused, but not with the stale-component message: %v", err)
	}
	for i, ev := range history {
		if ev.EventType == EventTypeDefer {
			t.Fatalf("event %d is a defer registration: the host ran before the refusal and recorded a defer for a run that then failed", i)
		}
	}
	if ops := operationsCalled(caller); len(ops) != 0 {
		t.Fatalf("the service saw %v from a refused component", ops)
	}
}
