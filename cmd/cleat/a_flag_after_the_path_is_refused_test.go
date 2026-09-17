package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1800. `cleat build <path> -o <dir>` silently ignored -o and wrote the
// artifact into the CURRENT DIRECTORY.
//
// flag.Parse stops at the first non-flag argument -- standard and documented --
// and everything after it lands in Args(), where the caller takes Args()[0] as
// the path and drops the rest. Exit 0, no message.
//
// WHY IT IS WORSE THAN AN IGNORED FLAG. -o names where output goes, so ignoring
// it does not degrade the run, it RELOCATES it. Two .wasm files landed in this
// repository's root during cleat#1811's example checks and `git add -A` swept
// one into a commit before it was caught.
func TestTrailingFlagsAreDetected(t *testing.T) {
	cases := []struct {
		name      string
		remainder []string
		want      []string
	}{
		{"the documented order leaves no remainder to check", []string{"./proj"}, nil},
		{"nothing at all", nil, nil},
		{"a flag after the path", []string{"./proj", "-o", "/tmp/out"}, []string{"-o"}},
		{"a long flag after the path", []string{"./proj", "--json"}, []string{"--json"}},
		{"two of them", []string{"./proj", "-o", "/tmp/out", "--json"}, []string{"-o", "--json"}},
		// The value of a flag is a positional-looking token and must not be
		// reported as a flag itself, or the message names something the user
		// did not write.
		{"a flag's VALUE is not a flag", []string{"./proj", "-o", "out"}, []string{"-o"}},
		// A bare "-" is stdin by convention, a positional wherever it is
		// accepted. Refusing it would break a working invocation.
		{"a bare dash is not a flag", []string{"./proj", "-"}, nil},
		{"a second positional is not a flag", []string{"./proj", "./other"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trailingFlags(tc.remainder)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("trailingFlags(%q) = %q, want %q", tc.remainder, got, tc.want)
			}
		})
	}
}

// TestTheReorderingSuggestionIsCorrect pins the message, which IS the feature:
// the behaviour being fixed is silence, so an unhelpful or wrong error is a
// smaller version of the same defect.
func TestTheReorderingSuggestionIsCorrect(t *testing.T) {
	got := flagAfterPositionalMessage("build", "./proj", []string{"-o", "/tmp/out"}, []string{"-o"})

	// The suggestion must be copy-pasteable. An earlier version re-emitted the
	// dropped flag NAMES followed by the path -- "cleat build -o ./proj" --
	// which loses -o's value and sets the output directory to the project.
	if !strings.Contains(got, "cleat build -o /tmp/out ./proj") {
		t.Errorf("the suggested command is not the input reordered:\n%s", got)
	}
	if strings.Contains(got, "cleat build -o ./proj") {
		t.Errorf("the suggestion dropped the flag's VALUE, which would set -o to the "+
			"project path:\n%s", got)
	}
	// The ./ escape is only advice where it applies. Appended unconditionally
	// it produced ".//Users/.../proj" for an absolute path.
	if strings.Contains(got, "begins with") {
		t.Errorf("the ./ escape was offered for a path that does not begin with \"-\":\n%s", got)
	}
	if esc := flagAfterPositionalMessage("build", "-weird", []string{"-o", "x"}, []string{"-o"}); !strings.Contains(esc, "./-weird") {
		t.Errorf("a path that DOES begin with \"-\" got no escape advice:\n%s", esc)
	}
}

// TestBuildRefusesAFlagAfterThePath is the reachability half. trailingFlags
// being correct and trailingFlags being CALLED are different claims, and the
// unit tests above pass whether or not main.go consults it -- the shape
// cleat#1756 is about.
//
// It also asserts the thing the issue is actually about: that nothing was
// written to the working directory.
func TestBuildRefusesAFlagAfterThePath(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	cwd := t.TempDir()
	out := t.TempDir()
	proj, err := filepath.Abs(filepath.Join("..", "..", "examples", "rust-workflow"))
	if err != nil {
		t.Fatalf("resolve the example: %v", err)
	}

	cmd := exec.Command(cleatBinary, "build", "--target", "rust", proj, "-o", out)
	cmd.Dir = cwd
	combined, err := cmd.CombinedOutput()
	got := string(combined)

	if err == nil {
		t.Errorf("the build exited 0 with -o after the path. It would have written the "+
			"artifact into the working directory (cleat#1800).\n\noutput:\n%s", got)
	}
	if !strings.Contains(got, "would be IGNORED") {
		t.Errorf("the refusal does not say the flag would be ignored, which is the whole "+
			"message.\n\noutput:\n%s", got)
	}
	// THE PROPERTY, not the message: nothing in the working directory.
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the working directory is not empty: %v. The build wrote somewhere the "+
			"user did not ask for.", names)
	}
}

// TestBuildStillAcceptsTheDocumentedOrder is the control. A refusal that fired
// on the correct order too would pass every assertion above.
func TestBuildStillAcceptsTheDocumentedOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	out := t.TempDir()
	proj := filepath.Join("..", "..", "examples", "rust-workflow")

	combined, err := exec.Command(cleatBinary, "build", "--target", "rust", "-o", out, proj).CombinedOutput()
	if err != nil {
		t.Fatalf("the documented order was refused: %v\n\noutput:\n%s", err, combined)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) == 0 {
		t.Errorf("the documented order produced no artifact in -o, so this control asserts "+
			"nothing.\n\noutput:\n%s", combined)
	}
}
