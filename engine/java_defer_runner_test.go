package engine

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// TestJavaExportsTheDeferRunner is IMPROVEMENT-PLAN §3.35 phase 4 for Java.
//
// §3.73 made the guest drain its own defer table when the entry point returns,
// which covers every workflow that gets to return. A workflow the HOST killed
// -- execution fence, instruction limit, memory ceiling -- never reaches the
// generated wrapper, so its cleanup would never happen at all: the lock stays
// held, the charge stays uncompensated.
//
// `runGuestDefersAfterKill` (engine/backend_wasmtime.go) looks the export up by
// name on the killed instance. Before this it returned nil for every Java
// guest and the host's kill-path cleanup silently did nothing.
//
// This goes through the real TeaVM build rather than the processor's generated
// source, because the two can disagree in the direction that matters.
// CleatEntryProcessorTest asserts the source contains `@Export`; only compiling
// it shows whether TeaVM kept it. Nothing in the guest calls this function --
// its only caller is the host, after the workflow is dead -- so it is exactly
// the shape dead-code elimination removes, and a tree-shaken export is
// indistinguishable from one that was never generated.
func TestJavaExportsTheDeferRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Java WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildJavaWasm(t))
	if err != nil {
		t.Fatalf("read Java WASM: %v", err)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	fn, ok := compiled.ExportedFunctions()[deferRunnerExport]
	if !ok {
		var names []string
		for n := range compiled.ExportedFunctions() {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("the Java module exports no %q; it exports %v.\n\n"+
			"CleatEntryProcessor generates CleatDeferRunner and CleatEntryIndex "+
			"references it to keep TeaVM from removing it. If the source is right "+
			"and this still fails, the reference chain from the analysis root is "+
			"broken and the export was tree-shaken.",
			deferRunnerExport, names)
	}

	// Signature, not just presence. The host calls this with no arguments and
	// reads an i64 count; anything else is found by name and then fails at the
	// call, which is a worse failure than not finding it.
	if got := len(fn.ParamTypes()); got != 0 {
		t.Errorf("%s takes %d parameter(s), want 0 -- the host calls it with none",
			deferRunnerExport, got)
	}
	if got := fn.ResultTypes(); len(got) != 1 || got[0] != api.ValueTypeI64 {
		t.Errorf("%s returns %v, want one i64 (how many bodies ran)",
			deferRunnerExport, got)
	}
}
