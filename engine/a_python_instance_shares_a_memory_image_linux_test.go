//go:build cgo && linux

package engine

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// A Python workflow instance shares a copy-on-write memory image instead of copying 10.6 MiB (cleat#4382).
//
// wasmtime builds that image only if the module's data segments cover at least half of the span they sit in, or the span is under 16 MiB. The
// CPython module componentize-py produces covers 49% of 21.7 MiB, so it got none and every instance paid about 12 MiB of private memory (measured
// as a fit over 1 to 64 instances, 2026-11-22). python-sdk/scripts/build_wasm.py now merges the segments into one. Nothing else notices if that step
// is lost, or if a new componentize-py lands the layout somewhere it no longer applies: the component still runs, 11 MiB heavier per instance.
//
// This builds the component with the real build script, takes the core module out of it, and counts the memory-image mappings that N instances add
// to this process. Linux only, by build tag: it reads /proc/self/maps, where an image is a mapping backed by a memfd.

// topLevelCoreModules returns the payload of every top-level core module (section id 1) of a component.
func topLevelCoreModules(t *testing.T, component []byte) [][]byte {
	t.Helper()
	uleb := func(b []byte, i int) (int, int) {
		r, s := 0, 0
		for {
			x := b[i]
			i++
			r |= int(x&0x7f) << s
			s += 7
			if x&0x80 == 0 {
				return r, i
			}
		}
	}
	if len(component) < 8 || string(component[:4]) != "\x00asm" {
		t.Fatalf("not a WebAssembly binary")
	}
	var mods [][]byte
	for i := 8; i < len(component); {
		id := component[i]
		size, j := uleb(component, i+1)
		if id == 1 {
			mods = append(mods, component[j:j+size])
		}
		i = j + size
	}
	return mods
}

func memfdMappings(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "memfd") {
			n++
		}
	}
	return n
}

func TestPythonAnInstanceSharesAMemoryImage(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	path := h.compileWorkflow(t, "all_host_calls_workflow.py", "all_host_calls_workflow")
	component, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}

	// The module CPython runs in is the one with the big memory: the largest core module in the component.
	var main []byte
	for _, m := range topLevelCoreModules(t, component) {
		if len(m) > len(main) {
			main = m
		}
	}
	if len(main) < 1<<20 {
		t.Fatalf("PRECONDITION FAILED: the largest core module in the component is %d bytes, so this is not the CPython module the test is about", len(main))
	}

	const n = 8
	// instantiate returns how many memory-image mappings n instances add, with a fresh engine so nothing is shared with an earlier call.
	instantiate := func(cow bool) int {
		cfg := wasmtime.NewConfig()
		cfg.SetMemoryInitCOWSet(cow)
		eng := wasmtime.NewEngineWithConfig(cfg)
		module, err := wasmtime.NewModule(eng, main)
		if err != nil {
			t.Fatalf("UNMEASURED: compiling the module: %v", err)
		}
		before := memfdMappings(t)
		var keep []*wasmtime.Store
		for i := 0; i < n; i++ {
			store := wasmtime.NewStore(eng)
			linker := wasmtime.NewLinker(eng)
			if err := linker.DefineUnknownImportsAsTraps(module); err != nil {
				t.Fatalf("UNMEASURED: stubbing imports: %v", err)
			}
			if _, err := linker.Instantiate(store, module); err != nil {
				t.Fatalf("UNMEASURED: instantiating: %v", err)
			}
			keep = append(keep, store)
		}
		got := memfdMappings(t) - before
		runtime.KeepAlive(keep)
		return got
	}

	// The control: with copy-on-write initialisation off no image exists, so the counter must read 0. A counter that cannot read 0 would make
	// "the instances share an image" a statement about nothing.
	if off := instantiate(false); off != 0 {
		t.Fatalf("PRECONDITION FAILED: %d image mappings with copy-on-write initialisation OFF; the counter cannot tell an image from no image", off)
	}
	if on := instantiate(true); on < n {
		t.Errorf("%d instances of the Python module added %d memory-image mappings, want at least %d: the module got no copy-on-write image, so every "+
			"instance copies its whole 10 MiB snapshot (about 12 MiB each instead of 1.3). Check that python-sdk/scripts/build_wasm.py still densifies the "+
			"module's data segments, and the build log above for 'Dense data' (cleat#4382)", n, on, n)
	}
}
