package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// TestGeneratedAdapterCompilesForEveryHostCall runs the real build pipeline over
// a workflow that calls every cleat.HostCalls method, and then hands the
// generated Go to the compiler.
//
// Nothing else in the tree does the last step. TestRunBuild_GoTargetBuildDir
// sits directly above this one and says so in its own comment -- "Verify the
// build directory setup for the go target without requiring actual go build to
// compile" -- and every test in ./wasm/ inspects the generated source as a
// string. A generated identifier that does not exist, or a closure whose
// signature cleat.HostCallsOptions will not accept, passes all of them.
//
// Four host calls shipped broken through that gap, each uncompilable from any
// Go WASM workflow (IMPROVEMENT-PLAN.md 3.204):
//
//   - AcquireLock / AcquireLockMs -- `undefined: ttl_ms`, and an AcquireLockMs
//     field that cleat.HostCallsOptions does not have
//   - CreatePromise / AwaitPromise -- `undefined: promise_idPtr`,
//     `timeout_ms`, `resultOutBuf`
//   - SideEffect -- closure emitted as func(fn func() (string, error)) where
//     the field is func(computedResult string)
//
// This test is slow by nature: it invokes the Go compiler for wasip1/wasm. It
// is worth the seconds. Do NOT make it skip when the toolchain looks
// unavailable -- wasip1 is part of the standard toolchain, and a skip here
// restores exactly the blind spot the test exists to remove.
func TestGeneratedAdapterCompilesForEveryHostCall(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the Go compiler; skipped under -short")
	}

	pattern := filepath.Join(testdataDir(t), "allhostcalls")
	result, _, _, _, usage, tr := analyze(pattern)
	if result == nil {
		t.Fatal("analyze returned nil for the allhostcalls package")
	}

	// The point of the fixture is breadth. If it stops covering the adapter
	// table the test still passes, so assert the breadth rather than assume it.
	if len(usage.Funcs) < 40 {
		t.Fatalf("allhostcalls exercised only %d host functions; it is supposed to "+
			"cover every one. Either the fixture lost calls or usage analysis "+
			"regressed -- both make this test vacuous.", len(usage.Funcs))
	}

	outDir := t.TempDir()
	goVersion := result.GoVersion
	if goVersion == "" {
		goVersion = "1.26"
	}
	cfg := &wasm.BuildConfig{
		SrcDir:      result.TargetPkg.Dir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  result.ModulePath,
		ProjectRoot: result.ModuleDir,
		GoVersion:   goVersion,
		Outputs:     wasm.BuildOutputs("main", usage, result, "go"),
		WASMOutput:  "entry.wasm",
		Target:      "go",
		XfrmSource:  tr.Files,
	}
	if err := wasm.PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	cmd := exec.Command("go", "build", "-o", filepath.Join(outDir, "entry.wasm"), ".")
	cmd.Dir = outDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated host adapter does not compile.\n"+
			"This is what the string-matching tests in ./wasm/ cannot see: an\n"+
			"identifier the generator emits that does not exist, or a closure\n"+
			"cleat.HostCallsOptions will not accept.\n\n"+
			"go build (GOOS=wasip1 GOARCH=wasm) in %s:\n%s\nerror: %v",
			outDir, out, err)
	}
}

// TestEveryHostCallIsExercisedByTheCompileFixture keeps the fixture honest.
//
// The compile test above is only as good as its input: a host call the fixture
// never mentions is a host call nobody compiles. adapterDefs is the table the
// generator emits from, so every FieldName in it must appear in the fixture.
func TestEveryHostCallIsExercisedByTheCompileFixture(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testdataDir(t), "allhostcalls", "main.go"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	text := string(src)

	missing := []string{}
	for _, name := range wasm.AdapterFieldNames() {
		if !strings.Contains(text, "h."+name+"(") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("testdata/allhostcalls/main.go does not call %d host method(s): %v\n"+
			"Every adapterDefs entry must be exercised, or the compile test does "+
			"not cover it. Add a call -- the result can be discarded with _.",
			len(missing), missing)
	}
}
