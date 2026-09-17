package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1782. The Rust determinism scan is strings.Contains over a line, and a
// comment or a string literal is a line like any other -- so prose NAMING a
// forbidden spelling was reported as a use of it, and since #1784 wired the
// scan into `cleat build --target rust` the same comment fails a BUILD.
//
// WHY A UNIT TEST AND NOT ONLY A FIXTURE. The fixture arm below proves the
// whole pipeline agrees; this proves what rustCodeOnly does, case by case, in
// milliseconds and without the cleat binary. The two answer different
// questions, and the fixture alone cannot say WHICH of the forms below is
// handled -- a scanner that blanked the entire file would pass it.
func TestRustCodeOnlyBlanksProseAndKeepsCode(t *testing.T) {
	cases := []struct {
		name string
		src  string
		gone []string // must NOT survive into the scanned text
		kept []string // must survive
	}{{
		name: "a line comment naming a pattern",
		src:  "// forbidden: use std::fs and std::net::TcpStream\nlet x = 1;\n",
		gone: []string{"use std::fs", "std::net::"},
		kept: []string{"let x = 1;"},
	}, {
		name: "a comment AFTER code on the same line -- the case a prefix test misses",
		src:  "let x = 1; // do not write use std::fs here\n",
		gone: []string{"use std::fs"},
		kept: []string{"let x = 1;"},
	}, {
		name: "a doc comment",
		src:  "/// Never call std::time::SystemTime::now in a workflow.\npub fn f() {}\n",
		gone: []string{"std::time::SystemTime::now"},
		kept: []string{"pub fn f() {}"},
	}, {
		name: "an inner doc comment",
		src:  "//! This crate must not use std::process::Command.\npub fn f() {}\n",
		gone: []string{"std::process::Command"},
		kept: []string{"pub fn f() {}"},
	}, {
		name: "a block comment",
		src:  "/* use std::thread::spawn is banned */\nlet y = 2;\n",
		gone: []string{"use std::thread"},
		kept: []string{"let y = 2;"},
	}, {
		name: "NESTED block comments -- Rust allows them and a depth counter is the difference",
		src:  "/* outer /* inner mentions use std::sync */ still comment, use std::fs */\nlet z = 3;\n",
		gone: []string{"use std::sync", "use std::fs"},
		kept: []string{"let z = 3;"},
	}, {
		name: "a string literal",
		src:  "let msg = \"do not use std::fs::read\";\n",
		gone: []string{"std::fs::"},
		kept: []string{"let msg ="},
	}, {
		name: "a raw string with hashes",
		src:  "let s = r#\"rand::thread_rng and std::net::UdpSocket\"#;\nlet t = 4;\n",
		gone: []string{"rand::", "std::net::"},
		kept: []string{"let t = 4;"},
	}, {
		name: "REAL CODE IS NOT TOUCHED -- the assertion that stops 'blank everything'",
		src:  "use std::fs;\nfn f() { let _ = std::fs::read(\"x\"); }\n",
		gone: nil,
		kept: []string{"use std::fs;", "std::fs::read"},
	}, {
		name: "a lifetime is not a character literal",
		src:  "fn f<'a>(s: &'a str) -> &'a str { s }\nuse std::fs;\n",
		gone: nil,
		kept: []string{"&'a str", "use std::fs;"},
	}, {
		name: "a character literal is blanked, and the code around it survives",
		src:  "let c = '\\n'; use std::fs;\n",
		gone: nil,
		kept: []string{"let c =", "use std::fs;"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(rustCodeOnly([]byte(tc.src)))

			// Positions are reported straight out of this result, so the
			// shape has to survive exactly.
			if len(got) != len(tc.src) {
				t.Fatalf("length changed: %d, want %d -- every reported line and "+
					"column would move", len(got), len(tc.src))
			}
			if strings.Count(got, "\n") != strings.Count(tc.src, "\n") {
				t.Fatalf("line count changed: %d, want %d",
					strings.Count(got, "\n"), strings.Count(tc.src, "\n"))
			}

			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("%q survived; the scan would report it as a use.\n  in: %q",
						g, got)
				}
			}
			for _, k := range tc.kept {
				if !strings.Contains(got, k) {
					t.Errorf("%q did NOT survive; the scan can no longer see real code.\n"+
						"  A blanker that eats everything passes every 'gone' case above\n"+
						"  and silently disables the checker.\n  in: %q", k, got)
				}
			}
		})
	}
}

// TestRustCodeOnlyStillSeesTheKnownPositive is the other half: the fixture the
// build gate refuses must still be visible through the blanker.
//
// Without this, "prose no longer matches" is equally consistent with "nothing
// matches any more", which is the failure direction -- the scan is a build gate
// since #1784 and a silent one emits artifacts it should refuse.
func TestRustCodeOnlyStillSeesTheKnownPositive(t *testing.T) {
	const src = "use std::fs;\n\n#[no_mangle]\npub fn workflow() {\n" +
		"    // reads a file, which the checker must still catch below\n" +
		"    let _ = std::fs::read_to_string(\"data.txt\");\n}\n"

	got := string(rustCodeOnly([]byte(src)))
	hits := 0
	for _, fb := range forbiddenRustPatterns {
		if fb.pattern == "" {
			continue
		}
		if strings.Contains(got, fb.pattern) {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("no forbidden pattern survives in code that plainly uses one:\n%s", got)
	}
}

// TestRustBuildAcceptsProseNamingAForbiddenPattern is the reachability half,
// and it is the one the unit tests above cannot supply.
//
// rustCodeOnly having correct behaviour and rustCodeOnly being CALLED are
// different claims. Reverting the call site in runVetRust leaves every unit
// test above green -- they drive the function directly -- so without this arm
// the change would ship as a function with tests and no caller, which is the
// exact shape cleat#1756 is about.
//
// It runs the real binary against a real crate, so it fails if the scan is
// unwired, if the fixture regresses, or if the build gate stops consulting vet.
func TestRustBuildAcceptsProseNamingAForbiddenPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	dir := filepath.Join("..", "..", "testdata", "vet-checks", "rust", "prose_names_a_pattern")
	// -o BEFORE the path. Written the other way round it was silently ignored
	// and the artifact would have gone to the working directory, which is
	// cmd/cleat/ under `go test` -- cleat#1800. This test is the instance the
	// refusal added there found in this repo's own suite.
	out, _ := exec.Command(cleatBinary, "build", "--target", "rust",
		"-o", t.TempDir(), dir).CombinedOutput()
	got := string(out)

	// ASSERT ON THE DETERMINISM CODES, not on the exit status. This fixture
	// keeps its source at the crate root, so cargo rejects the manifest and
	// the build exits non-zero for a reason that has nothing to do with the
	// checker -- the same trap rust_build_refuses_nondeterminism_test.go
	// records, where "the build fails" passed against a tree with no gate.
	for _, code := range []string{"R001", "R002", "R003", "R004", "R005", "R006", "R007"} {
		if strings.Contains(got, code) {
			t.Errorf("the build reported %s against a crate whose only mention of a "+
				"forbidden pattern is in comments and string literals (cleat#1782).\n\n"+
				"output:\n%s", code, got)
		}
	}
	if strings.Contains(got, "determinism check failed") {
		t.Errorf("the build refused a deterministic crate for determinism.\n\noutput:\n%s", got)
	}

	// The negative control for this arm: the run must have reached the vet
	// stage at all. Without it a binary that never ran, or a flag rename,
	// reports no codes and passes.
	if !strings.Contains(got, "Vetting Rust crate") {
		t.Fatalf("the vet stage did not run, so finding no determinism codes says "+
			"nothing.\n\noutput:\n%s", got)
	}
}
