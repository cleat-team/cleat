package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestJavaBuildRefusesNondeterminism is the Java half of what
// rust_build_refuses_nondeterminism_test.go asserts for Rust, and its doc
// comment carries the reasoning for the two-arm shape. The short version:
// either arm alone is satisfied by a tree where the gate does not exist, so
// they have to be asserted together, and the assertions are on TEXT because a
// build can fail for many reasons that are not determinism.
//
// # What changed here, and what did not
//
// cleat#1812 replaced the literal-spelling table (`forbiddenJavaPatterns`)
// with a resolver (`forbiddenJavaPaths`, `findForbiddenJavaPaths`) that
// resolves imports and fully-qualified names before matching, the same shape
// #1811 gave the Rust checker. TestFindForbiddenJavaPathsSeesWhatSpellingsMissed
// carries the form-by-form cases (static imports, wildcard imports, fully
// qualified calls, variable naming); this file stays at the build-integration
// level, one known-positive and one known-limit, unchanged in shape.
//
// java.nio remains the known limit -- the resolver fixed WHICH spellings of a
// listed API are seen, not WHICH APIs are listed, and java.nio.file was never
// on the list:
//
//	$ grep -c 'java\.nio' cmd/cleat/vet_java.go
//	0
//
// That is not a corner. java.nio.file is the API Java has recommended over
// java.io for filesystem work since 1.7, so the checker covers the legacy
// package and misses its replacement -- the escape is what a modern codebase
// would write first.
//
// # One asymmetry with the Rust side worth knowing
//
// vet_java.go SKIPS comment lines (via javaCodeOnly) and vet_rust.go does
// (via rustCodeOnly, the same shape). Verified behaviourally rather than by
// reading, because the Rust side taught that lesson the expensive way
// (cleat#1782): a Java comment naming System.currentTimeMillis() reports 0
// errors, while the same text as code reports 1. That is why this fixture's
// comment may quote the patterns it describes and the Rust fixture's may not.
func TestJavaBuildRefusesNondeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	// The fixture is COPIED to a temp directory before building, and the copy
	// is what gets built. gradle writes into its project directory, so
	// building the fixture in place leaves `build/reports/...` in the source
	// tree after every run -- which showed up as an untracked file about to be
	// committed. The Rust sibling does not need this today only because cargo
	// rejects that fixture's manifest before it creates `target/`, which is
	// luck rather than design.
	build := func(t *testing.T, fixture string) string {
		t.Helper()
		src := filepath.Join("..", "..", "testdata", "vet-checks", "java", fixture)
		dst := filepath.Join(t.TempDir(), fixture)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatalf("preparing the fixture copy: %v", err)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatalf("reading fixture %s: %v", src, err)
		}
		copied := 0
		for _, e := range entries {
			if e.IsDir() {
				continue // the fixtures are flat; nothing to recurse into
			}
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
				t.Fatalf("writing %s: %v", e.Name(), err)
			}
			copied++
		}
		// A fixture that copied nothing would build an empty directory and
		// fail for that reason, which is the "checks never started" shape:
		// the build would refuse and neither arm could tell why.
		if copied == 0 {
			t.Fatalf("copied 0 files from %s -- the fixture is missing or empty, "+
				"so neither arm below would be measuring the checker", src)
		}
		out, _ := exec.Command(cleatBinary, "build", "--target", "java", "-o", t.TempDir(), dst).CombinedOutput()
		return string(out)
	}

	// ARM 1 -- KNOWN POSITIVE. System.currentTimeMillis() is on the pattern
	// list, so the gate must refuse and must do so before gradle runs.
	t.Run("known positive is refused, and for the determinism reason", func(t *testing.T) {
		out := build(t, "e001_timestamp")

		if !strings.Contains(out, "J001") {
			t.Errorf("the build did not report the determinism finding.\n\n"+
				"A non-zero exit is NOT what this asserts: this fixture has no\n"+
				"TeaVM plugin configured, so gradle would fail on its own and the\n"+
				"build would exit non-zero with no gate present at all. The J001\n"+
				"code is what separates 'refused by the determinism check' from\n"+
				"'failed for an unrelated reason'.\n\noutput:\n%s", out)
		}
		if !strings.Contains(out, "no artifact was emitted") {
			t.Errorf("the build reported a determinism finding but did not say it "+
				"declined to emit an artifact.\n\noutput:\n%s", out)
		}
		if strings.Contains(out, "Compiling Java to WASM") {
			t.Errorf("the build reached the gradle stage despite a determinism "+
				"finding; the gate is running too late.\n\noutput:\n%s", out)
		}
	})

	// ARM 2 -- KNOWN LIMIT. java.nio.file does the same non-deterministic
	// thing and appears nowhere in the pattern table.
	t.Run("known limit escapes the checker, and says so out loud", func(t *testing.T) {
		out := build(t, "known_limit_nio")

		if strings.Contains(out, "determinism check failed") || strings.Contains(out, "Error [J0") {
			t.Errorf("the Java checker now CATCHES the java.nio fixture.\n\n"+
				"This is an improvement, not a regression. Three things to do:\n"+
				"  1. move this fixture to the known-positive arm above;\n"+
				"  2. write a new known-limit fixture for whatever still escapes\n"+
				"     -- do not leave this arm empty, or the next reader has no\n"+
				"     way to tell a strong checker from an unwired one;\n"+
				"  3. update LANGUAGE_SUPPORT.md, which describes Java\n"+
				"     enforcement as a substring scan.\n\noutput:\n%s", out)
		}
	})

	// ARMS 3-6 -- cleat#1812's own known positives: four cases the old
	// literal-spelling table could not see, each measured against it before
	// the resolver existed (see docs/contributor/design/java-determinism-checker.md
	// and each fixture's own comment for the measurement).
	positives := []struct {
		fixture, code string
	}{
		{"j001_static_import", "J001"},
		{"j004_fully_qualified_time", "J007"},
		{"j020_reflection", "J020"},
		{"naming_does_not_decide", "J015"},
	}
	for _, tc := range positives {
		t.Run(tc.fixture+" is refused, and for the determinism reason", func(t *testing.T) {
			out := build(t, tc.fixture)

			if !strings.Contains(out, tc.code) {
				t.Errorf("the build did not report %s for %s.\n\noutput:\n%s", tc.code, tc.fixture, out)
			}
			if !strings.Contains(out, "no artifact was emitted") {
				t.Errorf("the build reported a determinism finding but did not say it "+
					"declined to emit an artifact.\n\noutput:\n%s", out)
			}
		})
	}

	// ARM 7 -- MUST ALLOW. The old table refused this file with TWO false
	// positives on one line (J008 from "new java.io.", J016 from "InputStream"
	// as a substring of "ByteArrayInputStream"). A resolver that only ever
	// widens what is caught is not the property being asserted here -- this
	// arm is the one that catches a resolver refusing something fine.
	t.Run("pure_byte_array_stream is NOT refused", func(t *testing.T) {
		out := build(t, "pure_byte_array_stream")

		if strings.Contains(out, "determinism check failed") || strings.Contains(out, "Error [J0") {
			t.Errorf("the build refused a pure, in-memory byte stream.\n\noutput:\n%s", out)
		}
	})
}
