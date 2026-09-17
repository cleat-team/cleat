package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1789. The determinism gate walked every .rs file under the crate,
// including sources that are never compiled into the cdylib it guards. Since
// #1784 wired it into `cleat build --target rust`, a test that reads a fixture
// refused the BUILD -- and reading a fixture is what a test is for, so this
// fired on the first project with an integration test rather than on an
// unusual coincidence.
//
// Two mechanisms, because one does not reach the other's case:
//
//	tests/ benches/ examples/   Cargo target directories, skipped by the walk
//	#[cfg(test)]                a module inside a file the gate MUST scan
//
// The second is the commoner form and no directory rule can reach it.
func TestRustVetSkipsSourcesThatAreNotTheArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	vet := func(t *testing.T, fixture string) string {
		t.Helper()
		dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", fixture)
		out, _ := exec.Command(cleatBinary, "vet", "--lang", "rust", dir).CombinedOutput()
		return string(out)
	}

	t.Run("a crate whose tests are non-deterministic still vets clean", func(t *testing.T) {
		out := vet(t, "test_sources_are_not_the_artifact")

		for _, code := range []string{"R001", "R002", "R003", "R004", "R005", "R006", "R007"} {
			if strings.Contains(out, code) {
				t.Errorf("reported %s. Every forbidden spelling in this fixture is in a "+
					"tests/ target, a benches/ target, or a #[cfg(test)] module -- none of "+
					"which reaches the cdylib (cleat#1789).\n\noutput:\n%s", code, out)
			}
		}
		// The denominator. "No codes" is also what a run that read no files
		// prints, and the walk change is precisely what could cause that.
		if !strings.Contains(out, "1 files") {
			t.Errorf("expected the scan to read the crate's ONE library source; a count of 0 "+
				"means the skip ate the crate rather than its test targets.\n\noutput:\n%s", out)
		}
	})

	// THE CONTROL, and it is the half that stops "skip everything" passing.
	// A blanker that ran away -- brace matching that overshoots the cfg(test)
	// module, or a directory rule that matched the crate root -- reports no
	// codes on the fixture above and no codes here either.
	t.Run("real code in src/ is still refused", func(t *testing.T) {
		out := vet(t, "e001_fs_access")
		if !strings.Contains(out, "R001") {
			t.Errorf("the known positive is no longer reported, so the change above did not "+
				"narrow the scan -- it disabled it.\n\noutput:\n%s", out)
		}
	})
}

// TestWithoutCfgTestBlanksTheModuleAndNothingAfterIt pins the brace matching,
// which is the part a fixture cannot localise: if it stops at the first `}`
// instead of the module's own, the fixture still vets clean and the failure
// only shows up as a MISSED finding in some later file.
func TestWithoutCfgTestBlanksTheModuleAndNothingAfterIt(t *testing.T) {
	// THE ORDER INSIDE THE MODULE IS THE WHOLE ASSERTION. `inner` closes a
	// brace before the module does, and the forbidden spelling sits AFTER
	// that close. Written the other way round -- the spelling first -- the
	// case passes whether the matching finds the module's brace or stops at
	// the first one, because everything before the first close is blanked
	// either way. That draft was written, and a mutation that stopped early
	// went green against it.
	const src = "pub fn before() { let _ = 1; }\n" +
		"#[cfg(test)]\n" +
		"mod tests {\n" +
		"    fn inner() { if true { let _ = 1; } }\n" +
		"    use std::fs;\n" +
		"}\n" +
		"pub fn after() { use std::net; let _ = 2; }\n"

	got := string(withoutCfgTest([]byte(src)))

	if len(got) != len(src) {
		t.Fatalf("length changed: %d, want %d -- every reported line and column would move",
			len(got), len(src))
	}
	if strings.Contains(got, "use std::fs") {
		t.Errorf("the cfg(test) module was not blanked:\n%s", got)
	}
	if !strings.Contains(got, "pub fn before()") {
		t.Errorf("code BEFORE the module was blanked:\n%s", got)
	}
	if !strings.Contains(got, "use std::net") {
		t.Errorf("code AFTER the module was blanked, so the matching ran past the module's "+
			"closing brace. That HIDES findings in whatever follows.\n%s", got)
	}
}

// TestWithoutCfgTestLeavesABodylessItemAlone pins the over-reporting direction.
// `#[cfg(test)] mod tests;` has no brace to match; blanking to the next `{`
// found anywhere would swallow whatever item comes next.
func TestWithoutCfgTestLeavesABodylessItemAlone(t *testing.T) {
	const src = "#[cfg(test)]\nmod tests;\npub fn real() { use std::fs; }\n"

	got := string(withoutCfgTest([]byte(src)))
	if !strings.Contains(got, "use std::fs") {
		t.Errorf("a bodyless cfg(test) item swallowed the following function, which is the "+
			"direction that HIDES findings:\n%s", got)
	}
}

// TestRustVetAnchorsTheTargetSkipAtTheCrateRoot pins the other half of the
// skip, which the fixture above cannot: Cargo's tests/benches/examples
// convention is POSITIONAL, so a rule matching the directory name alone reports
// real library code as absent rather than as clean.
//
// src/tests/ is an ordinary module compiled into the cdylib. Its violation is a
// finding, and a name-only skip never reads the file.
func TestRustVetAnchorsTheTargetSkipAtTheCrateRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", "a_library_module_named_tests")
	out, _ := exec.Command(cleatBinary, "vet", "--lang", "rust", dir).CombinedOutput()
	got := string(out)

	if !strings.Contains(got, "R001") {
		t.Errorf("src/tests/mod.rs is library code and reads a file, but no finding was "+
			"reported. A directory rule matching the NAME rather than the position "+
			"skipped it (cleat#1789).\n\noutput:\n%s", got)
	}
	// The denominator again: both library sources must have been read.
	if !strings.Contains(got, "2 files") {
		t.Errorf("expected both src/lib.rs and src/tests/mod.rs to be scanned.\n\noutput:\n%s", got)
	}
}
