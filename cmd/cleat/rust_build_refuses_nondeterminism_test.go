package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRustBuildRefusesNondeterminism asserts that `cleat build --target rust`
// runs the determinism checker and refuses to emit an artifact when it finds
// something -- and, in the same run, states what the checker does not find.
//
// # Why both arms are in one test
//
// Either arm alone is satisfied by a tree where the gate does not exist.
//
//   - The known-limit arm asserts a build is NOT refused. An unwired gate
//     never refuses anything, so it passes that arm perfectly.
//   - The known-positive arm asserts a build IS refused -- and a refusal is
//     not evidence on its own, because a build can fail for a hundred reasons
//     that have nothing to do with determinism.
//
// That second one is not hypothetical. Measured on develop before this gate
// existed, `cleat build --target rust` on the e001_fs_access fixture already
// exited 1:
//
//	$ cleat build --target rust testdata/vet-checks/rust/e001_fs_access
//	error: failed to parse manifest ... no targets specified in the manifest
//	Error: cargo build failed: exit status 101
//	exit=1     mentions of R001 or "non-deterministic": 0
//
// The fixture keeps its sources at the crate root rather than under src/, so
// cargo rejects the manifest. A test asserting only "the build fails on a
// non-deterministic fixture" therefore PASSED against a tree with no gate at
// all -- the issue's own acceptance criterion, satisfied by an unrelated
// error. Hence: assert on the TEXT, and keep the arm that can only pass when
// something declines to refuse.
//
// # Why the known-limit arm does not assert an exit status
//
// The known-limit fixture passes the gate and then reaches cargo, which fails
// on the same crate-root layout. That exit status says nothing about the
// checker, and making the fixture cargo-buildable would couple a test about a
// SUBSTRING MATCHER to a working Rust toolchain. The property under test is
// "the gate did not refuse this", which is textual and needs no toolchain.
func TestRustBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	build := func(t *testing.T, fixture string) string {
		t.Helper()
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", fixture)
		out, _ := exec.Command(cleatBinary, "build", "--target", "rust", "-o", t.TempDir(), dir).CombinedOutput()
		return string(out)
	}

	// ARM 1 -- KNOWN POSITIVE. A plain single-module import of the standard
	// filesystem module is on forbiddenRustPatterns, so the gate must refuse
	// the build and must do so BEFORE cargo runs: an artifact that was never
	// compiled cannot be deployed by accident.
	t.Run("known positive is refused, and for the determinism reason", func(t *testing.T) {
		out := build(t, "e001_fs_access")

		if !strings.Contains(out, "R001") {
			t.Errorf("the build did not report the determinism finding.\n\n"+
				"A non-zero exit is NOT what this asserts -- this fixture's crate\n"+
				"layout makes cargo fail on its own, so the build already exited 1\n"+
				"before any gate existed. The R001 code is what distinguishes\n"+
				"'refused by the determinism check' from 'failed for some other\n"+
				"reason'.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build reported a determinism finding but did not say it "+
				"declined to emit an artifact.\n\noutput:\n%s", out)
		}
		// The gate runs before the cargo lookup, so a refused build never
		// reaches the compiler. If this string appears, the gate is running
		// too late and a partial artifact may exist.
		if strings.Contains(out, "Compiling Rust WASM module") {
			t.Errorf("the build reached the cargo stage despite a determinism "+
				"finding; the gate is running too late.\n\noutput:\n%s", out)
		}
	})

	// ARM 2 -- KNOWN LIMIT. The same non-determinism, spelled with a grouped
	// import, is invisible to a substring matcher. This arm exists so that
	// "5 of 5 languages refuse a bad fixture" cannot be read as parity of
	// ENFORCEMENT between a whole-program analysis and a list of strings.
	//
	// It is also a tripwire: if the Rust checker is ever strengthened, this
	// goes red. That is good news, not a regression -- see the failure text.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		out := build(t, "known_limit_grouped_use")

		if strings.Contains(out, "determinism check failed") || strings.Contains(out, "Error [R0") {
			t.Errorf("the Rust checker now CATCHES the grouped-import fixture.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to the known-positive arm above;\n"+
				"  2. write a new known-limit fixture for whatever still escapes\n"+
				"     -- do not leave this arm empty, or the next reader has no\n"+
				"     way to tell a strong checker from an unwired one;\n"+
				"  3. update LANGUAGE_SUPPORT.md, which describes Rust\n"+
				"     enforcement as a substring scan.\n\noutput:\n%s", out)
		}
	})
}
