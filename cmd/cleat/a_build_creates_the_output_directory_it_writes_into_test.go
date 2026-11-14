package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#3792, item 6. `cleat build --target python -o out hello.py` compiled,
// printed `Build SUCCESS ... Metadata stamped successfully.` and then exited 1
// with `Error: writing WASM output to out/hello.wasm: open ...: no such file or
// directory` when `out` did not exist. The Go build creates what it needs; the
// Java, Rust, AssemblyScript and Python builds write into the directory
// themselves and assumed it was there.
//
// The property is "the directory exists before the target-specific work", so
// the test points each target at a project that cannot build (an empty
// directory) and asserts on the directory rather than on the build's verdict:
// the toolchains (componentize-py, npx, cargo, a JDK) need not be present for
// it to mean something, and a build that fails for want of one still must not
// have skipped this step.
func TestEveryTargetCreatesTheOutputDirectoryItWritesInto(t *testing.T) {
	if cleatBinary == "" {
		t.Fatal("TestMain did not build the cleat binary (short mode?); this test measures nothing without it")
	}
	for _, target := range []string{"java", "assemblyscript", "rust", wasm.PythonTarget} {
		t.Run(target, func(t *testing.T) {
			proj := t.TempDir()
			out := filepath.Join(t.TempDir(), "does", "not", "exist")
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("precondition: %s should not exist yet (stat err %v)", out, err)
			}
			// The build is expected to fail: the project is empty. Its exit status
			// is not the subject.
			cmd := exec.Command(cleatBinary, "build", "--target", target, "-o", out, proj)
			cmd.Run()
			if fi, err := os.Stat(out); err != nil || !fi.IsDir() {
				t.Errorf("cleat build --target %s -o %s left no output directory (stat: %v)", target, out, err)
			}
		})
	}
}
