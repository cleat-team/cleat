package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestThePythonVetSaysWhetherItRan asserts that `cleat vet --lang python`
// distinguishes "this file has violations" from "I could not check this file",
// and that neither is reported as success.
//
// # Why the third outcome needs its own status
//
// Before cleat#1801 there were two, and they were the wrong two: a vet that
// could not run exited 1, and a vet that found violations exited 0. So the
// only signal a caller got was inverted with respect to the file. `cleat vet
// --lang python && cleat deploy` was a false green on a non-deterministic
// workflow, and would have refused to deploy a perfectly good one on a machine
// whose interpreter was too old.
//
// The contract now:
//
//	0  inspected, clean
//	1  inspected, has violations   -- go and look at them
//	2  could NOT be inspected      -- the check is broken, the file may be fine
//
// 0 and 2 must differ because a vet that could not look agrees with every file.
// 1 and 2 must differ because the build gate this feeds (cleat#1770) has to say
// "I could not check this" rather than "this is non-deterministic" -- a message
// naming the wrong problem is worse than no message at all.
//
// # The arms are the same fixture under different preconditions
//
// Every arm runs the identical directory and the identical file. The only
// difference is whether the vet could run at all. That is what makes the exit
// codes mean something: a control that changed the file too would show that
// the numbers differ without showing why.
//
// # The could-not-run condition is CONSTRUCTED, and the first version was not
//
// The first version of this test made cleat_sdk unreachable by emptying
// PYTHONPATH. That works on a developer machine and FAILED IN CI, because the
// Tier 1 job runs `pip install .` in python-sdk, so cleat_sdk is in
// site-packages and no PYTHONPATH setting can hide it. The arm got exit 1
// (violations) where it asserted 2, on a tree where nothing was wrong.
//
// So the arm was not testing "the vet could not run". It was testing "this
// machine happens not to have the SDK installed" -- an environmental accident
// that held where it was written and nowhere else.
//
// Both preconditions below are now built by the test rather than hoped for,
// and they cover the two DIFFERENT branches that reach the same verdict:
//
//	no python3 on PATH at all      -> the error is not an ExitError    (!isExit)
//	a python3 that cannot import   -> exit 1 with EMPTY STDOUT         (stdout.Len() == 0)
//
// The second is the subtle one and the reason runVetPython has a discriminator
// at all: cleat_sdk.vet exits 1 BOTH when it finds violations and when it
// cannot be imported, so the exit status alone cannot separate them. Stdout
// can: a run that inspected the file produces output either way.
//
// The stub reproduces that signature exactly. Verified against the real thing
// on 2026-09-17: a genuine ModuleNotFoundError, on a machine without the SDK
// installed, produces the same exit 2 through the same branch.
func TestThePythonVetSaysWhetherItRan(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	// If the interpreter is too old, BOTH arms return 2 and the test would pass
	// while measuring nothing -- the violations arm would be reporting an
	// import failure, not a clean run. Skip rather than keep an arm that cannot
	// distinguish what it is named for. CI pins 3.12, so nothing is lost there.
	if v, ok := pythonAtLeast(pythonSDKMinVersion); !ok {
		t.Skipf("python3 is %s; the Python SDK requires >= %s, so the violations arm "+
			"could not be told apart from the could-not-run arm", v, pythonSDKMinVersion)
	}

	// A copy, in a directory with no python-sdk beside it. findPythonSDKDir
	// resolves the SDK from the binary's directory and the cwd, so running
	// this from the repo root would find it regardless of PYTHONPATH -- which
	// is exactly what happened on the first attempt at this test, and the
	// could-not-run arm quietly returned 1.
	dir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open", "workflow.py")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	fixture := filepath.Join(dir, "py002_open")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "workflow.py"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// pathDir: what to put on PATH ("" means inherit). pythonPath: PYTHONPATH.
	runWithPath := func(t *testing.T, pathDir, pythonPath string) (int, string) {
		t.Helper()
		cmd := exec.Command(cleatBinary, "vet", "--lang", "python", "py002_open")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonPath)
		if pathDir != "" {
			cmd.Env = append(cmd.Env, "PATH="+pathDir)
		}
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return 0, string(out)
		case errors.As(err, &exitErr):
			return exitErr.ExitCode(), string(out)
		default:
			t.Fatalf("running cleat vet: %v\n%s", err, out)
			return -1, ""
		}
	}

	t.Run("no interpreter at all reports UNMEASURED, not a clean file", func(t *testing.T) {
		// PATH with nothing on it: exec cannot find python3, so cmd.Run returns
		// an error that is not an *exec.ExitError.
		code, out := runWithPath(t, t.TempDir(), filepath.Join(repoRoot(t), "python-sdk"))

		if code == vetExitOK {
			t.Errorf("exit 0 when cleat_sdk could not be imported.\n\n"+
				"A vet that could not look agrees with every file, correct or not. "+
				"That is the failure this exit code exists to prevent.\n%s", out)
		}
		if code == vetExitViolations {
			t.Errorf("exit %d (violations) when the vet could not run at all.\n\n"+
				"This sends the reader to look at a file that may be perfectly fine, "+
				"and a build gate reading this status would refuse the build with a "+
				"message naming the wrong problem.\n%s", code, out)
		}
		// Each UNMEASURED arm asserts that the message names ITS OWN
		// precondition, not UNMEASURED in general. Both exit 2, so the status
		// alone cannot tell the reader whether to install an interpreter or
		// fix PYTHONPATH -- the text is the only thing that can.
		if !strings.Contains(out, "python3") {
			t.Errorf("the failure did not name python3, so the reader cannot tell "+
				"which precondition failed.\n%s", out)
		}
	})

	t.Run("an interpreter that cannot import the SDK reports UNMEASURED, not violations", func(t *testing.T) {
		// A python3 that reproduces the exact signature runVetPython keys on:
		// exit 1, NOTHING on stdout, a traceback on stderr. That is what a real
		// `ModuleNotFoundError: No module named 'cleat_sdk'` looks like, and it
		// is indistinguishable by exit status alone from "found 2 violations".
		stubDir := t.TempDir()
		stub := "#!/bin/sh\n" +
			"echo 'Traceback (most recent call last):' >&2\n" +
			"echo \"ModuleNotFoundError: No module named 'cleat_sdk'\" >&2\n" +
			"exit 1\n"
		if err := os.WriteFile(filepath.Join(stubDir, "python3"), []byte(stub), 0o755); err != nil {
			t.Fatalf("writing the python3 stub: %v", err)
		}

		code, out := runWithPath(t, stubDir, filepath.Join(repoRoot(t), "python-sdk"))

		if code == vetExitOK {
			t.Errorf("exit 0 when the interpreter could not import cleat_sdk.\n\n"+
				"A vet that could not look agrees with every file, correct or not.\n%s", out)
		}
		if code == vetExitViolations {
			t.Errorf("exit %d (violations) when the vet never inspected the file.\n\n"+
				"This is the case the stdout discriminator exists for: cleat_sdk.vet\n"+
				"exits 1 both for violations and for a failed import, so only the\n"+
				"absence of stdout separates them. Reporting it as violations sends\n"+
				"the reader to a file that may be fine, and a build gate reading this\n"+
				"would refuse with a message naming the wrong problem.\n%s", code, out)
		}
		if !strings.Contains(out, "cleat_sdk") {
			t.Errorf("the failure did not name cleat_sdk, so the reader cannot tell "+
				"which precondition failed.\n%s", out)
		}
	})

	t.Run("a vet that ran and found violations reports them in its exit status", func(t *testing.T) {
		// CONTROL for the arm above: identical directory, identical file, the
		// SDK reachable. If this returns 2 the other arm proves nothing,
		// because both arms would be reporting the same broken environment.
		code, out := runWithPath(t, "", filepath.Join(repoRoot(t), "python-sdk"))

		if code == vetExitUnmeasured {
			t.Fatalf("the control arm could not run the vet either (exit %d), so the "+
				"UNMEASURED arm above is not evidence of anything.\n%s", code, out)
		}
		if code == vetExitOK {
			t.Errorf("exit 0 for py002_open, a fixture built specifically to contain a "+
				"violation, whose own report says otherwise.\n\n"+
				"This is cleat#1801: the report and the exit status disagreed, so "+
				"`cleat vet --lang python && deploy` was a false green.\n%s", out)
		}
	})
}
