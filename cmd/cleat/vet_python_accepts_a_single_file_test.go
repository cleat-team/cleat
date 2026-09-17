package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVetPythonAcceptsTheSingleFileForm covers cleat#1825.
//
// runVetPython called os.ReadDir before checking whether its argument was
// itself a .py file, so the form the flag's help text documents -- and the form
// a build gate needs, because a build is about one file rather than the
// directory around it -- died on "cannot read directory workflow.py: not a
// directory" and never reached the branch written to handle it.
//
// Both forms are asserted in one test. The directory arm is not decoration: the
// repair moves ReadDir into an else branch, and a repair that broke the
// directory form while fixing the file form would otherwise go unnoticed.
func TestVetPythonAcceptsTheSingleFileForm(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if !pythonVetRunnable(t) {
		t.Skip("python3 on PATH cannot import cleat_sdk.vet (the SDK needs >= 3.10)")
	}

	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py004_time_sleep")

	t.Run("a .py file is analysed, not treated as a directory", func(t *testing.T) {
		cmd := exec.Command(cleatBinary, "vet", "--lang", "python",
			filepath.Join(fixture, "workflow.py"))
		cmd.Env = pythonSDKPath()
		out, err := cmd.CombinedOutput()
		got := string(out)

		if strings.Contains(got, "cannot read directory") {
			t.Fatalf("the file form still reports the directory error:\n%s", got)
		}
		// py004 violates PY004, so a clean exit would mean it analysed nothing.
		if err == nil {
			t.Errorf("vet exited 0 on a fixture that violates PY004:\n%s", got)
		}
		if !strings.Contains(got, "PY004") {
			t.Errorf("vet did not report the fixture's violation, so it did not analyse the "+
				"file it was handed:\n%s", got)
		}
	})

	t.Run("the directory form still works", func(t *testing.T) {
		cmd := exec.Command(cleatBinary, "vet", "--lang", "python", fixture)
		cmd.Env = pythonSDKPath()
		out, _ := cmd.CombinedOutput()
		got := string(out)

		if strings.Contains(got, "cannot read directory") {
			t.Errorf("the repair broke the directory form:\n%s", got)
		}
		if !strings.Contains(got, "PY004") {
			t.Errorf("the directory form no longer reports the fixture's violation:\n%s", got)
		}
	})
}

// pythonSDKPath points the child at the in-repo SDK. findPythonSDKDir resolves
// python-sdk/ relative to the working directory, and `go test` runs in
// cmd/cleat/, so it finds nothing there.
func pythonSDKPath() []string {
	sdk, err := filepath.Abs(filepath.Join("..", "..", "python-sdk"))
	if err != nil {
		return os.Environ()
	}
	return append(os.Environ(), "PYTHONPATH="+sdk)
}

// pythonVetRunnable asks the question the test depends on -- can python3 import
// cleat_sdk.vet -- rather than a proxy for it such as the version number. The
// version is the usual cause (PEP 604 unions are a TypeError before 3.10) but
// an uninstalled SDK produces the same inability with a different reason.
func pythonVetRunnable(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("python3", "-c", "import cleat_sdk.vet")
	cmd.Env = pythonSDKPath()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("python3 cannot import cleat_sdk.vet: %v\n%s", err, out)
		return false
	}
	return true
}
