//go:build cgo

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// The compiled-size multiplier is re-derived from the checked-in artifacts.
//
// # Why this asserts a RATIO and not a size
//
// The artifacts are build outputs. They get rebuilt, and a toolchain bump
// moves every absolute number in the table by some amount that means nothing
// here. What the bound depends on is the RELATIONSHIP between wasm length and
// compiled size, and that is what this pins.
//
// # What it would catch
//
// A wasmtime upgrade that changed code generation enough to push real
// artifacts past the multiplier. That would not fail anything else: the cache
// would simply under-count itself and a deployment sized against
// --wasm-module-cache-max-mb would hold more than it was told. Silent
// over-retention is exactly the failure cleat#1907 was filed about, one layer
// down.
//
// # Why the bar is "no LARGE artifact exceeds it"
//
// Not "every artifact", because two classes deliberately sit outside and both
// are safe (see CompiledSizeEstimateMultiplier): tiny AssemblyScript modules
// run 5x-9x but cost tens of kilobytes, and debug builds run 0.2x so the
// estimate over-counts them. Failing on those would be failing on the two
// cases the multiplier was chosen to tolerate.
func TestTheCompiledSizeEstimateStillHolds(t *testing.T) {
	// Large enough that the ratio matters to the bound: under this, a 3x
	// error is measured in kilobytes.
	const significant = 100 << 10

	cfg := wasmtime.NewConfig()
	cfg.SetEpochInterruption(true)
	eng := wasmtime.NewEngineWithConfig(cfg)

	root := repoRootForEstimate(t)
	candidates := []string{
		"examples/rust-workflow/target/wasm32-wasip1/release/rust_workflow.wasm",
		"examples/rust-all-host-calls/target/wasm32-wasip1/release/rust_all_host_calls.wasm",
		"examples/java-workflow/build/wasm/wasm/workflow.wasm",
		"examples/saga-java-port/build/wasm/wasm/workflow.wasm",
		"tests/plugin-harness/testdata/hostcallsrust/target/wasm32-unknown-unknown/release/hostcallsrust.wasm",
		"tests/plugin-harness/testdata/hostcallsjava/build/wasm/wasm/workflow.wasm",
		"tests/plugin-harness/testdata/javaworkflow/prebuilt/workflow.wasm",
		"tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm",
	}

	checked := 0
	for _, rel := range candidates {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			// A build output that has not been built is not a failure here.
			// The vacuity guard below is what stops that hiding everything.
			continue
		}
		if len(body) < significant {
			continue
		}

		var serialized []byte
		if mod, merr := wasmtime.NewModule(eng, body); merr == nil {
			serialized, err = mod.Serialize()
		} else {
			// call_all_plugins is a COMPONENT, and NewModule refuses it with
			// "expected a WebAssembly module but was given a WebAssembly
			// component". It is also the artifact that matters most to this
			// bound -- 19 MB of wasm, 46 MB compiled -- so it is compiled the
			// other way rather than skipped.
			comp, cerr := wasmtime.NewComponent(eng, body)
			if cerr != nil {
				t.Errorf("%s compiles as neither module nor component: %v / %v", rel, merr, cerr)
				continue
			}
			serialized, err = comp.Serialize()
		}
		if err != nil {
			t.Errorf("%s: serialize: %v", rel, err)
			continue
		}

		checked++
		ratio := float64(len(serialized)) / float64(len(body))
		estimate := CompiledSizeEstimate(len(body))
		if int64(len(serialized)) > estimate {
			t.Errorf("%s: compiled to %d bytes from %d (%.1fx), above the %dx estimate of %d.\n\n"+
				"The cache would under-count this artifact, so a deployment sized against "+
				"--wasm-module-cache-max-mb would hold more than it was told. Either the "+
				"multiplier needs raising against fresh measurements, or code generation "+
				"changed and the table in CompiledSizeEstimateMultiplier is stale.",
				rel, len(serialized), len(body), ratio, CompiledSizeEstimateMultiplier, estimate)
		}
	}

	// THE VACUITY GUARD. Every artifact above is a build output, so a tree
	// where none has been built would check nothing and pass -- which is how a
	// guard over build outputs quietly stops guarding.
	if checked == 0 {
		t.Skip("no built artifact above the significance threshold; nothing to re-derive from")
	}
}

// repoRootForEstimate finds the checkout from this package's directory.
func repoRootForEstimate(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "examples")); err == nil {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot locate the repository root from %s", dir)
	return ""
}
