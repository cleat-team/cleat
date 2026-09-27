package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAnUnreadablePathIsNotAFinding asserts that `cleat vet --lang python`
// distinguishes "this workflow has violations" from "I could not look at
// anything", for the two paths that resolve no files.
//
// # Why this is separable from cleat#1825
//
// #1825 made the single-file branch reachable: os.ReadDir ran first, so passing
// the entry file died on "cannot read directory". That was an ordering bug and
// the fix is not in dispute.
//
// This is a different claim, and someone could reasonably argue with it on its
// own: an unreadable path is not a determinism finding. Both used to return 1,
// which under cleat#1801's contract means "inspected, has violations" and sends
// the reader to look for non-determinism in a file the checker never opened.
//
//	0  inspected, clean
//	1  inspected, has violations
//	2  could NOT be inspected -- the check is broken, the file may be fine
//
// # The ordering fix made this the LOUDER half, not a tidy-up
//
// Before #1825 a mistyped path died at os.ReadDir. After it, the single-file
// check runs first, so a mistyped path falls through to the empty-file-list
// branch instead. Landing the ordering fix moved this from a dormant wrong exit
// code to the live one. Noted by cleat-review, who wrote #1825, while I had it
// filed as follow-up tidying.
func TestAnUnreadablePathIsNotAFinding(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the python vet over temp directories")
	}

	t.Run("a path that does not exist is UNMEASURED", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-directory")
		assertUnmeasured(t, missing, "a path that does not exist")
	})

	t.Run("a directory with no .py files is UNMEASURED", func(t *testing.T) {
		empty := t.TempDir()
		assertUnmeasured(t, empty, "a directory containing no .py files")
	})

	// FLOOR. Both arms above assert a specific non-zero value, which is also
	// what a vet that had broken entirely would produce. A real violation in a
	// real file must still come back as 1, or the arms are measuring a
	// uniformly-failing checker rather than a discriminating one.
	t.Run("a real violation is still reported as a finding", func(t *testing.T) {
		dir := t.TempDir()
		src := "from cleat_sdk import cleat_entry, HostCalls\n\n\n" +
			"@cleat_entry\ndef workflow(h: HostCalls, input: str) -> str:\n" +
			"    return open(\"data.txt\").read()\n"
		if err := os.WriteFile(filepath.Join(dir, "workflow.py"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := runVetPython(dir, false); code != vetExitViolations {
			// Not a hard failure when the interpreter cannot run the SDK at
			// all: that is the UNMEASURED case doing its job, and it is a
			// precondition of this test rather than a finding about it.
			if code == vetExitUnmeasured {
				t.Skipf("the python vet could not run here (exit %d); the arms above "+
					"cannot be distinguished from a uniformly broken checker, so this "+
					"test proves nothing in this environment", code)
			}
			t.Errorf("a workflow doing file I/O returned %d, want %d (violations).\n\n"+
				"Without this, the UNMEASURED arms above would pass equally well "+
				"against a vet that had stopped discriminating entirely.",
				code, vetExitViolations)
		}
	})
}

func assertUnmeasured(t *testing.T, path, what string) {
	t.Helper()
	code := runVetPython(path, false)
	switch code {
	case vetExitOK:
		t.Errorf("%s returned 0.\n\nA vet that could not look agrees with every "+
			"workflow, correct or not -- which is the failure this exit code exists "+
			"to prevent.", what)
	case vetExitViolations:
		t.Errorf("%s returned %d (violations).\n\nNothing was inspected, so this says "+
			"nothing about any workflow. It sends the reader to look for "+
			"non-determinism in a file the checker never opened, and a build gate "+
			"reading this status refuses with a message naming the wrong problem.",
			what, code)
	case vetExitUnmeasured:
		// correct
	default:
		t.Errorf("%s returned %d, which is none of the three documented outcomes", what, code)
	}
}
