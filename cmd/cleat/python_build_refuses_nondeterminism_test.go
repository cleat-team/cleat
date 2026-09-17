package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPythonBuildRefusesNondeterminism is the last of the four languages in
// cleat#1770. The two-arm reasoning is in rust_build_refuses_nondeterminism_test.go
// and is unchanged: either arm alone is satisfied by a tree where the gate does
// not exist, and the assertions are on TEXT because a build can fail for many
// reasons that are not determinism.
//
// # What is different about Python
//
// Its checker is the strongest of the four non-Go ones -- a real AST call-graph
// and closure analyser rather than a substring scan -- and it still works from
// a list of known module names, so the limit is a missing NAME rather than a
// missing spelling.
//
// The gate could not simply be wired. Until cleat#1801, runVetPython returned 0
// for every violating file, so `if code != 0` would have produced a gate that
// was present, green and inert -- the exact shape cleat#1770 exists to remove.
// Wiring it also required fixing a second defect: runVetPython's single-file
// branch was unreachable, because os.ReadDir ran first and returned on any
// non-directory, so passing the entry file the build is about to compile always
// failed with "cannot read directory".
//
// # Three outcomes, two refusals, one decision
//
// The gate refuses on BOTH a finding and an unrunnable check, and says which.
// The decision is the same because emitting an unchecked artifact is what the
// gate exists to prevent; the message differs because "I could not check this"
// and "this is non-deterministic" send the reader to different places.
//
// Refusing on UNMEASURED is a deliberate departure from detectEntryFunction
// just above it, which falls back to a line scan when the SDK is missing "so
// the build still works without the SDK". That is right for entry detection --
// guessing the entry point wrong fails loudly at deploy. It is wrong for a
// determinism gate, where guessing "deterministic" wrong fails silently on
// replay, possibly much later. Same absence, opposite consequence.
func TestPythonBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	// On an interpreter the SDK cannot be imported by, EVERY arm is refused as
	// UNMEASURED and the test would pass while measuring nothing -- the
	// known-limit arm would be reporting an import failure, not a clean run.
	// Skip rather than keep arms that cannot distinguish what they are named
	// for. CI pins 3.10/3.11/3.12, so nothing is lost there.
	if v, ok := pythonAtLeast(pythonSDKMinVersion); !ok {
		t.Skipf("python3 is %s; the Python SDK requires >= %s, so both arms would be "+
			"refused as UNMEASURED and neither would measure the checker", v, pythonSDKMinVersion)
	}

	build := func(t *testing.T, fixture string) string {
		t.Helper()
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "python", fixture)
		cmd := exec.Command(cleatBinary, "build", "--target", "python", "-o", t.TempDir(), dir)
		cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot(t), "python-sdk"))
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	// ARM 1 -- KNOWN POSITIVE. open() is PY002, on the checker's list.
	t.Run("known positive is refused, and for the determinism reason", func(t *testing.T) {
		out := build(t, "py002_open")

		if !strings.Contains(out, "PY002") {
			t.Errorf("the build did not report the determinism finding.\n\n"+
				"A non-zero exit is not what this asserts: a python build can fail\n"+
				"for a missing componentize-py, an unimportable SDK, or a bad entry\n"+
				"point. The PY002 code is what separates 'refused by the determinism\n"+
				"check' from 'failed for an unrelated reason'.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build reported a determinism finding but did not say it "+
				"declined to emit an artifact.\n\noutput:\n%s", out)
		}
		// The gate runs before the componentize-py lookup, so a refused build
		// never reaches the compiler.
		if strings.Contains(out, "componentize") {
			t.Errorf("the build reached the componentize-py stage despite a determinism "+
				"finding; the gate is running too late.\n\noutput:\n%s", out)
		}
		// A finding must not be reported as a broken check. Both refuse, and
		// conflating them sends the reader to fix an interpreter when the
		// problem is their workflow.
		if strings.Contains(out, "failure of the CHECK") {
			t.Errorf("a real determinism finding was reported as a failure of the "+
				"check itself.\n\noutput:\n%s", out)
		}
	})

	// ARM 2 -- UNMEASURED refuses too, and says something DIFFERENT.
	//
	// Both a finding and an unrunnable check refuse the build, because emitting
	// an unchecked artifact is what this gate exists to prevent. But they send
	// the reader to different places, so the messages differ: "your workflow is
	// non-deterministic" against "I could not check your workflow".
	//
	// Without this arm the gate could collapse to `if code != 0` and every test
	// above would still pass -- which is how it was written in an earlier
	// independent implementation of this same gate (#1809, closed). A message
	// naming the wrong problem is worse than no message, and a user on an
	// interpreter that cannot import cleat_sdk would be told their code is at
	// fault.
	t.Run("a check that could not run refuses, but not as a determinism finding", func(t *testing.T) {
		// A PATH with no python3 on it at all: the vet cannot run, which is
		// vetExitUnmeasured rather than a verdict about the fixture.
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open")
		cmd := exec.Command(cleatBinary, "build", "--target", "python", "-o", t.TempDir(), dir)
		cmd.Env = append(os.Environ(),
			"PYTHONPATH="+filepath.Join(repoRoot(t), "python-sdk"),
			"PATH="+t.TempDir())
		raw, _ := cmd.CombinedOutput()
		out := string(raw)

		if !strings.Contains(out, "could not run") {
			t.Errorf("a vet that could not run was not reported as such.\n\noutput:\n%s", out)
		}
		if strings.Contains(out, "determinism check failed") {
			t.Errorf("an unrunnable check was reported as a determinism FINDING.\n\n"+
				"py002_open does contain a violation, but the vet never looked at it "+
				"here -- there is no interpreter. Reporting this as non-determinism "+
				"tells a user with a broken toolchain that their workflow is at "+
				"fault.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build did not say it declined to emit an artifact. A check "+
				"that could not look must not produce an unchecked artifact.\n\n"+
				"output:\n%s", out)
		}
	})

	// ARM 3 -- KNOWN LIMIT, and it is a shape rather than a missing name.
	//
	// vet.py's forbidden-API table is keyed on exact (module, function) pairs
	// and matched only against a ONE-LEVEL `module.func()` call. The fixture
	// calls the very function the table contains -- ("time", "time"), PY005 --
	// spelled so the matcher cannot see it:
	//
	//	import time;           time.time()   -> PY005, caught
	//	from time import time; time()        -> 0 errors, the fixture
	//
	// Same function, same non-determinism, one import statement apart. That is
	// why this fixture is better than a module the table has never heard of: it
	// cannot be dismissed as an omission from a list, because the list has the
	// entry and the call still escapes. Three more shapes escape the same way
	// (two-level chains like datetime.datetime.now() and os.path.exists(), and
	// untabled functions like os.getpid()) -- see the fixture comment.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		out := build(t, "known_limit_bare_import")

		if strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the Python checker now CATCHES the datetime fixture.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to the known-positive arm above;\n"+
				"  2. write a new known-limit fixture -- pathlib.Path.read_text()\n"+
				"     and secrets.token_hex() both escaped as of 2026-09-17, so\n"+
				"     there is somewhere to go. Do not leave this arm empty, or\n"+
				"     the next reader cannot tell a strong checker from one that\n"+
				"     never ran;\n"+
				"  3. update LANGUAGE_SUPPORT.md, which describes Python\n"+
				"     enforcement as AST analysis over a list of known modules.\n\noutput:\n%s", out)
		}
	})
}
