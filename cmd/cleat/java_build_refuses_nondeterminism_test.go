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
// # What is different about Java, and why the known limit is a better one
//
// forbiddenJavaPatterns is far longer than Rust's -- around thirty entries
// against Rust's dozen -- and covers java.io, java.net, java.sql, java.time
// and java.util.concurrent. It is still substring matching, and length is not
// strength: it does not mention java.nio anywhere.
//
//	$ grep -c 'java\.nio' cmd/cleat/vet_java.go
//	0
//
// That is not a corner. java.nio.file is the API Java has recommended over
// java.io for filesystem work since 1.7, so the checker covers the legacy
// spelling and misses its replacement -- the escape is what a modern codebase
// would write first. A list of forbidden strings ages against the language it
// is checking, and nothing about it fails when it does.
//
// # One asymmetry with the Rust side worth knowing
//
// vet_java.go SKIPS comment lines (`vet_java.go:118-123`) and vet_rust.go does
// not. Verified behaviourally rather than by reading, because the Rust side
// taught that lesson the expensive way (cleat#1782): a Java comment naming
// System.currentTimeMillis() reports 0 errors, while the same text as code
// reports 1. That is why this fixture's comment may quote the patterns it
// describes and the Rust fixture's may not.
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
		out, _ := exec.Command(cleatBinary, "build", "--target", "java", dst,
			"-o", t.TempDir()).CombinedOutput()
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
}
