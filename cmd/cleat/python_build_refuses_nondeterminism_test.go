package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pythonSDKEnv points the child at the in-repo SDK.
//
// findPythonSDKDir resolves python-sdk/ relative to the working directory, and
// `go test` runs in cmd/cleat/, so it finds nothing there. Setting PYTHONPATH
// is what runVetPython's own failure hint tells a user to do, and it keeps
// these tests about the GATE rather than about SDK discovery.
func pythonSDKEnv() []string {
	sdk, err := filepath.Abs(filepath.Join("..", "..", "python-sdk"))
	if err != nil {
		return os.Environ()
	}
	return append(os.Environ(), "PYTHONPATH="+sdk)
}

// pythonVetUsable reports whether this machine can run the checker AT ALL.
//
// A GENUINE ENVIRONMENTAL PRECONDITION, not a crash wearing a skip's clothes.
// It asks the question the tests actually depend on -- can python3 import
// cleat_sdk.vet -- rather than a proxy for it. An earlier version checked only
// `sys.version_info >= (3,10)` and reported "usable" on a machine where the
// module was not importable, so every test failed with a toolchain error dressed
// up as a determinism finding.
//
// The version is still the usual cause: python-sdk uses PEP 604 unions
// (`X | None`) at module scope, which are a TypeError at import time before
// 3.10, and runVetPython prints a hint saying exactly that.
func pythonVetUsable(t *testing.T) bool {
	t.Helper()
	cmd := exec.Command("python3", "-c", "import cleat_sdk.vet")
	cmd.Env = pythonSDKEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("python3 on PATH cannot import cleat_sdk.vet: %v\n%s", err, out)
		return false
	}
	return true
}

// TestPythonBuildRefusesNondeterminism asserts that `cleat build --target
// python` runs the determinism checker and refuses to emit an artifact when it
// finds something -- and, in the same run, that it does NOT refuse a file the
// checker is happy with.
//
// # Why both arms are in one test
//
// This is the shape TestRustBuildRefusesNondeterminism establishes, for the
// same reason: either arm alone is satisfied by a tree where the gate does not
// exist.
//
//   - The deterministic arm asserts a build is NOT refused. An unwired gate
//     refuses nothing and passes that arm perfectly.
//   - The refusing arm asserts a build IS refused -- and a refusal alone is no
//     evidence, because `cleat build --target python` fails for plenty of
//     reasons that have nothing to do with determinism. On a machine without
//     componentize-py it fails for that, every time.
//
// So the refusing arm asserts on TEXT, and the deterministic arm asserts the
// refusal text is ABSENT while the checker's own summary is present -- which is
// what distinguishes "the gate ran and passed it" from "the gate never ran".
func TestPythonBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if !pythonVetUsable(t) {
		t.Skip("python3 on PATH cannot import the SDK (needs >= 3.10)")
	}

	const refusal = "determinism check failed"

	t.Run("a non-deterministic file is refused, and for the right reason", func(t *testing.T) {
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py004_time_sleep")
		cmd := exec.Command(cleatBinary, "build", "--target", "python",
			"-o", t.TempDir(), filepath.Join(dir, "workflow.py"))
		cmd.Env = pythonSDKEnv()
		out, err := cmd.CombinedOutput()
		got := string(out)

		if err == nil {
			t.Errorf("build succeeded on a workflow calling time.sleep in a durable function:\n%s", got)
		}
		if !strings.Contains(got, refusal) {
			t.Errorf("build failed, but not at the determinism gate -- so this would pass "+
				"against a tree with no gate. Output:\n%s", got)
		}
		if !strings.Contains(got, "PY004") && !strings.Contains(got, "time.sleep") {
			t.Errorf("the refusal does not name what it found, so a user cannot act on it:\n%s", got)
		}
	})

	t.Run("a deterministic file is not refused by the gate", func(t *testing.T) {
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "python", "known_limit_deterministic")
		cmd := exec.Command(cleatBinary, "build", "--target", "python",
			"-o", t.TempDir(), filepath.Join(dir, "workflow.py"))
		cmd.Env = pythonSDKEnv()
		out, _ := cmd.CombinedOutput()
		got := string(out)

		// No exit-status assertion: past the gate this reaches componentize-py,
		// whose absence says nothing about the checker. The property under test
		// is "the gate did not refuse this", which is textual.
		if strings.Contains(got, refusal) {
			t.Errorf("the gate refused a file the checker reports no errors for:\n%s", got)
		}
		if !strings.Contains(got, "0 errors") {
			t.Errorf("no checker summary in the output, so there is no evidence the gate ran "+
				"at all -- which is the reading this arm exists to rule out:\n%s", got)
		}
	})
}

// TestVetPythonReportsFindingsInItsExitStatus covers the defect that made the
// gate above inert when it was first wired.
//
// runVetPython fell through to printing when cleat_sdk.vet exited 1 with
// findings, and only ever set its own exit code on a TOOLING failure or on
// non-empty stderr. Findings go to stdout. So `cleat vet --lang python`
// printed a report ending "1 errors" and exited 0, and every caller keying on
// its status -- a CI step, the build gate -- was measuring nothing. Observed
// before the fix:
//
//	PY006: random.random() is not allowed in durable functions
//	Summary: 1 functions, 1 in durable closure, 1 errors, 0 warnings
//	exit=0
func TestVetPythonReportsFindingsInItsExitStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if !pythonVetUsable(t) {
		t.Skip("python3 on PATH cannot import the SDK (needs >= 3.10)")
	}

	file := filepath.Join("..", "..", "testdata", "vet-checks", "python",
		"py004_time_sleep", "workflow.py")
	cmd := exec.Command(cleatBinary, "vet", "--lang", "python", file)
	cmd.Env = pythonSDKEnv()
	out, err := cmd.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, "errors") {
		t.Fatalf("vet produced no report, so this test is measuring the wrong failure:\n%s", got)
	}
	if err == nil {
		t.Errorf("vet reported findings and exited 0, so nothing keying on its status can "+
			"act on them:\n%s", got)
	}
}

// TestVetPythonAcceptsASingleFile covers the ordering bug the gate surfaced:
// runVetPython called os.ReadDir before checking whether its argument was
// itself a .py file, so the single-file form -- the one the build gate uses,
// and the one the flag's help text shows -- died on "cannot read directory".
func TestVetPythonAcceptsASingleFile(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if !pythonVetUsable(t) {
		t.Skip("python3 on PATH cannot import the SDK (needs >= 3.10)")
	}

	file := filepath.Join("..", "..", "testdata", "vet-checks", "python",
		"known_limit_deterministic", "workflow.py")
	cmd := exec.Command(cleatBinary, "vet", "--lang", "python", file)
	cmd.Env = pythonSDKEnv()
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "cannot read directory") {
		t.Errorf("vet treated a .py file as a directory:\n%s", got)
	}
	if !strings.Contains(got, "0 errors") {
		t.Errorf("vet did not analyse the file it was given:\n%s", got)
	}
}
