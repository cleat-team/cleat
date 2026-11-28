package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// cleat#3501. The bytes `cleat build` produces must depend on the source and
// the toolchain, not on where the output was written or what the surrounding
// checkout looks like. Two things leaked, and each has its own test below:
//
//   - the -o directory's path. The build compiles inside -o, and without
//     -trimpath the toolchain recorded that path (DWARF, panic file names, the
//     main module's path);
//   - the version-control stamp. With -o inside a git checkout, Go's default
//     -buildvcs=auto records vcs.revision, vcs.time and vcs.modified, so the
//     bytes changed on every commit, including one that touched an unrelated
//     file. clew builds to -o dist in-repo, which is the ordinary case.
//
// Either would make a byte-comparing guard (cleat#3497's proposed refusal under
// open runs) refuse a redeploy of unchanged source.

// buildFixture writes a one-entry workflow module into a fresh directory.
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/cleat-team/cleat/cleat"

// @cleatEntry(name="hello")
func Hello(h cleat.HostCalls, input string) (string, error) {
	return `+"`"+`{"greeting":"hello"}`+"`"+`, nil
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	modContent := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
		"replace github.com/cleat-team/cleat => " + root + "\n" +
		"replace github.com/cleat-team/cleat/cleat => " + filepath.Join(root, "cleat") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return dir
}

// buildSum runs `cleat build -o out .` from dir and returns the sha256 of the
// one .wasm it wrote.
func buildSum(t *testing.T, dir, out string) string {
	t.Helper()
	// Run from the package directory, as the sibling build tests do: the test
	// process itself sits inside the repo's go.work.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cleatBinary, "build", "-o", out, ".")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build -o %s failed:\n%s", out, b)
	}
	wasms, _ := filepath.Glob(filepath.Join(out, "*.wasm"))
	if len(wasms) != 1 {
		t.Fatalf("want exactly one .wasm under %s, got %v", out, wasms)
	}
	b, err := os.ReadFile(wasms[0])
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func needBinary(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}
}

func TestABuildDoesNotEmbedTheOutputPath(t *testing.T) {
	needBinary(t)
	dir := buildFixture(t)

	// Two output directories whose paths differ in content AND length, so a
	// path embedded anywhere in the binary changes its bytes.
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "a-considerably-longer-output-directory-name")
	sa, sb := buildSum(t, dir, a), buildSum(t, dir, b)
	if sa != sb {
		t.Errorf("one source tree built to two output paths gave different bytes:\n  %s  %s\n  %s  %s", sa, a, sb, b)
	}
}

func TestABuildInsideAGitCheckoutDoesNotEmbedTheCommit(t *testing.T) {
	needBinary(t)
	dir := buildFixture(t)

	git := func(args ...string) {
		t.Helper()
		// -c rather than a config file, and no signing: the test must not
		// depend on, or write to, the developer's git configuration.
		full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "one")

	out := filepath.Join(dir, "dist")
	first := buildSum(t, dir, out)

	// A commit that adds an unrelated file. The workflow's source is unchanged,
	// so its bytes must be too. dist is removed first so the tree is clean at
	// the second commit exactly as it was at the first.
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "two")
	second := buildSum(t, dir, out)

	if first != second {
		t.Errorf("an unrelated commit changed the bytes of an unchanged workflow built with -o inside the checkout:\n"+
			"  commit one: %s\n  commit two: %s\n"+
			"Go stamps vcs.revision/vcs.time/vcs.modified unless -buildvcs=false (cleat#3501).", first, second)
	}
}
