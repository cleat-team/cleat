package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/internal/closure"
	"github.com/cleat-team/cleat/internal/transform"
)

// TestDropAutoThreaded is the unit half of the fix for IMPROVEMENT-PLAN 3.229.
//
// VerifyThreading reports the PRE-TRANSFORM state on purpose --
// TestVerifyThreadingAutothreadReportsPassThroughErrors says so in its own
// comment: pass-through functions in a global-h package "are correctly reported
// as unthreaded BEFORE the transform runs. After the transform they get h added
// as a parameter."
//
// `cleat build` treated that report as fatal, so it rejected packages the very
// next stage was designed to fix.
func TestDropAutoThreaded(t *testing.T) {
	errs := []closure.ThreadingError{
		{FuncName: "pkg.passThrough", Message: "no HostCalls parameter"},
		{FuncName: "pkg.genuinelyUnreachable", Message: "no HostCalls parameter"},
	}
	tr := &transform.Result{AddedH: []string{"pkg.passThrough"}}

	got := dropAutoThreaded(errs, tr)
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining error, got %d: %+v", len(got), got)
	}
	if got[0].FuncName != "pkg.genuinelyUnreachable" {
		t.Errorf("dropped the wrong one: %s survived", got[0].FuncName)
	}

	// A function the transform did NOT touch must still fail the build.
	if len(dropAutoThreaded(errs, &transform.Result{})) != 2 {
		t.Error("with no auto-threading, every error must survive")
	}
	if len(dropAutoThreaded(errs, nil)) != 2 {
		t.Error("a nil transform result must not swallow errors")
	}
}

// TestBuildAutoThreadedPackage is the end-to-end half.
//
// testdata/autothread declares a package-level `var h cleat.HostCalls` and has
// two pass-through functions that never reference it. Before 3.229 this failed
// with "validateAndReserve is reachable from a workflow entry point ... but does
// not have a HostCalls parameter" -- telling the author to declare the global
// the package already declares.
//
// It exercises the whole path, which is the point: the check reports, the build
// gate filters, the transform auto-threads, and the emitted Go has to COMPILE.
// The last step is what caught the second defect (3.230): the transform renamed
// the SDK import to "durable" and broke every `cleat.X` reference in the file.
// No unit test on any single stage would have seen that.
func TestBuildAutoThreadedPackage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain on PATH, yet `go test` is executing this: %v", err)
	}
	outDir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "autothread")

	cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build failed on a package with a global var h:\n%s\n%v", out, err)
	}

	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("reading build output: %v", readErr)
	}
	found := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("build reported success but produced no .wasm:\n%s", out)
	}
}
