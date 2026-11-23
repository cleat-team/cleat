//go:build cgo && linux

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// A Python workflow instance shares a copy-on-write memory image instead of copying 10.6 MiB (cleat#4382).
//
// wasmtime builds that image only if the module's data segments cover at least half of the span they sit in, or the span is under 16 MiB. The
// CPython module componentize-py produced for the first measurement covered 49% of 21.7 MiB, so it got none and every instance paid about 12 MiB of
// private memory (a fit over 1 to 64 instances, 2026-11-22). python-sdk/scripts/build_wasm.py now merges the segments into one.
//
// TWO TESTS, because they claim different things and one cannot stand in for the other. componentize-py's layout sits within a point of the 50% line
// and moves with the build environment: cleat-review2 measured 51.1% in its image on the same example and 49% was measured here, so on some builds
// the rewrite is a no-op and a real-build test goes green without the rewrite having done anything.
//
//   - TestPythonAnInstanceSharesAMemoryImage is the PRODUCT property: the component `build_wasm.py` ships gets a shared image, whatever mechanism
//     gave it one. It says in its log whether the rewrite acted.
//   - TestTheDenseRewriteMakesWasmtimeBuildAnImageForASparseModule is the MECHANISM: a module with CPython's shape (many small segments, 33% dense, a
//     span over 16 MiB) gets no image from wasmtime, and does once the rewrite has run. It does not depend on componentize-py's layout.
//
// Linux only, by build tag: both read /proc/self/maps, where an image is a mapping backed by a memfd.

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

// imageMappingsAdded returns how many memory-image mappings n instances of module add to this process, on a fresh engine so nothing is shared with
// an earlier call.
func imageMappingsAdded(t *testing.T, module []byte, cow bool, n int) int {
	t.Helper()
	cfg := wasmtime.NewConfig()
	cfg.SetMemoryInitCOWSet(cow)
	eng := wasmtime.NewEngineWithConfig(cfg)
	m, err := wasmtime.NewModule(eng, module)
	if err != nil {
		t.Fatalf("UNMEASURED: compiling the module: %v", err)
	}
	before := memfdMappings(t)
	var keep []*wasmtime.Store
	for i := 0; i < n; i++ {
		store := wasmtime.NewStore(eng)
		linker := wasmtime.NewLinker(eng)
		if err := linker.DefineUnknownImportsAsTraps(m); err != nil {
			t.Fatalf("UNMEASURED: stubbing imports: %v", err)
		}
		if _, err := linker.Instantiate(store, m); err != nil {
			t.Fatalf("UNMEASURED: instantiating: %v", err)
		}
		keep = append(keep, store)
	}
	got := memfdMappings(t) - before
	runtime.KeepAlive(keep)
	return got
}

func needPython(t *testing.T, h *pythonWasmTestHelper) {
	t.Helper()
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
}

const imageInstances = 8

func TestPythonAnInstanceSharesAMemoryImage(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	needPython(t, h)

	// Build as compileWorkflow does, but keep the output: whether the rewrite ACTED is part of what this test reports.
	output := filepath.Join(t.TempDir(), "all_host_calls_workflow.wasm")
	cmd := exec.Command("python3", filepath.Join(h.sdkRoot, "scripts", "build_wasm.py"),
		"--entry", filepath.Join(h.sdkRoot, "examples", "all_host_calls_workflow.py")+":all_host_calls_workflow", "--output", output)
	cmd.Dir = h.sdkRoot
	cmd.Env = append(os.Environ(), "PYTHONPATH="+h.sdkRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build_wasm.py failed:\n%s\n%v", out, err)
	}
	// The step must have RUN, whatever it decided: every outcome prints one "Dense data:" line. A build that never calls the step prints none, which on
	// a layout that is already dense would otherwise pass the image check below with nothing having been tested (cleat-review2's mutant P1).
	if !strings.Contains(string(out), "Dense data:") {
		t.Fatalf("the build printed no 'Dense data:' line, so build_wasm.py never ran the dense-data step:\n%s", out)
	}
	acted := regexp.MustCompile(`Dense data: ([\d,]+) segments over ([\d.]+) MiB covered ([\d.]+) MiB \((\d+)%\)`).FindStringSubmatch(string(out))
	if acted != nil {
		t.Logf("REWRITE ACTED: the CPython module had %s segments covering %s MiB of %s MiB (%s%%), so wasmtime would have built no image", acted[1], acted[3], acted[2], acted[4])
	}

	component, err := os.ReadFile(output)
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

	// The control: with copy-on-write initialisation off no image exists, so the counter must read 0. A counter that cannot read 0 would make
	// "the instances share an image" a statement about nothing.
	if off := imageMappingsAdded(t, main, false, imageInstances); off != 0 {
		t.Fatalf("PRECONDITION FAILED: %d image mappings with copy-on-write initialisation OFF; the counter cannot tell an image from no image", off)
	}
	on := imageMappingsAdded(t, main, true, imageInstances)
	if on >= imageInstances && acted == nil {
		t.Logf("REWRITE DID NOT ACT: this build's CPython module was already dense enough for wasmtime. This green result shows the shipped component " +
			"gets an image, not that the rewrite made it so; TestTheDenseRewriteMakesWasmtimeBuildAnImageForASparseModule is what pins the rewrite.")
	}
	if on < imageInstances {
		t.Errorf("%d instances of the Python module added %d memory-image mappings, want at least %d: the module got no copy-on-write image, so every "+
			"instance copies its whole 10 MiB snapshot (about 12 MiB each instead of 1.3). The rewrite acted on this build: %v. Check that "+
			"python-sdk/scripts/build_wasm.py still densifies the module's data segments (cleat#4382)", imageInstances, on, imageInstances, acted != nil)
	}
}

// sparseModuleScript builds, with the rewrite's own code, a module shaped like CPython's (600 segments of 12 KB spaced 40 KB apart plus one far
// segment: 7.2 MB of data over a 22 MiB span, 33%) and its rewritten form, writing both to the two paths it is given.
const sparseModuleScript = `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("bw", sys.argv[1]); bw = importlib.util.module_from_spec(spec); spec.loader.exec_module(bw)
def section(sid, payload): return bytes([sid]) + bw._uleb(len(payload)) + payload
def segment(off, data): return bw._uleb(0) + b"\x41" + bw._sleb(off) + b"\x0b" + bw._uleb(len(data)) + data
segs = [(1024 + i * 40000, bytes([i % 251 + 1]) * 12000) for i in range(600)] + [(22 << 20, b"\xaa" * 28000)]
mod = b"\x00asm\x01\x00\x00\x00" + section(1, b"\x01\x60\x00\x00") + section(5, b"\x01\x00" + bw._uleb(400)) + section(12, bw._uleb(len(segs))) \
    + section(11, bw._uleb(len(segs)) + b"".join(segment(o, d) for o, d in segs))
res = bw.densify_module(mod)
# A rewrite that declines (returns None) leaves the module as it was; the Go side then sees no image and says so, rather than this script failing.
dense = res[0] if res is not None else mod
open(sys.argv[2], "wb").write(mod); open(sys.argv[3], "wb").write(dense)
`

func TestTheDenseRewriteMakesWasmtimeBuildAnImageForASparseModule(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	if _, err := exec.LookPath("python3"); err != nil {
		if toolchainRequired("python") {
			t.Fatalf("python3 is missing, but %s declares python", requireToolchainEnv)
		}
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	sparse, dense := filepath.Join(dir, "sparse.wasm"), filepath.Join(dir, "dense.wasm")
	if out, err := exec.Command("python3", "-c", sparseModuleScript, filepath.Join(h.sdkRoot, "scripts", "build_wasm.py"), sparse, dense).CombinedOutput(); err != nil {
		t.Fatalf("UNMEASURED: building the modules: %v\n%s", err, out)
	}
	sparseBytes, err1 := os.ReadFile(sparse)
	denseBytes, err2 := os.ReadFile(dense)
	if err1 != nil || err2 != nil {
		t.Fatalf("UNMEASURED: reading the modules: %v %v", err1, err2)
	}

	// The known-positive: wasmtime really does leave this layout without an image. If it did not (a wasmtime that handles sparse layouts), the
	// rewrite would be unneeded and the claim below would be vacuous, which is a result worth failing loudly for.
	if got := imageMappingsAdded(t, sparseBytes, true, imageInstances); got != 0 {
		t.Fatalf("PRECONDITION FAILED: wasmtime built an image (%d mappings) for the SPARSE module, so the rewrite is no longer what makes the difference; "+
			"if wasmtime handles sparse layouts now, python-sdk/scripts/build_wasm.py's densify step can go (cleat#4382)", got)
	}
	// The control: the counter reads 0 when copy-on-write is off, so the positive result below is an image and not some other memfd.
	if off := imageMappingsAdded(t, denseBytes, false, imageInstances); off != 0 {
		t.Fatalf("PRECONDITION FAILED: %d mappings with copy-on-write initialisation OFF", off)
	}
	if on := imageMappingsAdded(t, denseBytes, true, imageInstances); on < imageInstances {
		t.Errorf("the rewritten module added %d memory-image mappings for %d instances, want at least %d: the rewrite no longer makes wasmtime build an image", on, imageInstances, imageInstances)
	}
}
