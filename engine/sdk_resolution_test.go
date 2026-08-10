package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSDKResolvesToThisTree is the guard on go.work.
//
// The root go.mod neither requires nor replaces the cleat/ SDK. No production
// package under the root module imports it -- that is checked separately, by
// TestRootModuleDoesNotDependOnSDK below -- but the root module's *test*
// fixtures do: testdata/ is 21 directories of workflow code written the way a
// user writes it, which is the only honest way to test a tool whose job is
// compiling exactly that. go.work is what supplies the SDK to those tests, from
// ./cleat in this tree.
//
// A `require` was deliberately NOT added for them. It would work, and it would
// be worse: with a require, building outside the workspace resolves the SDK to
// whatever pseudo-version the proxy last published, and every drift test here
// then compares the tree against a published snapshot of itself -- the
// engine<->SDK call-error contract test, the whole analyzer stack, the generated
// adapters. All of them would go green having measured a release rather than the
// working tree, with no warning. That is the failure CLAUDE.md opens with.
// Without the require, the same situation is a build error naming the missing
// package.
//
// This test is the belt to that braces, and it must FAIL rather than skip when
// the workspace is absent. `go list -m` exiting non-zero ("not a known
// dependency") is precisely the symptom, so treating it as an environmental
// skip would hide the one thing being checked.
func TestSDKResolvesToThisTree(t *testing.T) {
	repoRoot := repoRootFrom(t)

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}",
		"github.com/cleat-team/cleat/cleat").CombinedOutput()
	if err != nil {
		t.Fatalf("the cleat/ SDK is not in this build's module graph:\n  %s\n"+
			"Root-module tests get it from the workspace, so this means the workspace "+
			"is off (GOWORK=off, or a build from outside the repo). Unset GOWORK and "+
			"run from the repo root; see go.work.", strings.TrimSpace(string(out)))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatal("the cleat/ SDK module has no local directory, which means it was " +
			"resolved from the module cache rather than from this tree. See go.work.")
	}

	want := filepath.Join(repoRoot, "cleat")
	got, err := filepath.EvalSymlinks(dir)
	if err != nil {
		got = dir
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		wantResolved = want
	}
	if got != wantResolved {
		t.Errorf("the cleat/ SDK resolves to %s, not this tree's %s.\n"+
			"Every test that compares the engine against the SDK is then comparing "+
			"this tree against a published snapshot of it, and passes without "+
			"measuring anything. Check GOWORK and go.work.", got, wantResolved)
	}
}

// TestRootModuleDoesNotDependOnSDK is the other half, and the one that keeps
// the module cycle broken.
//
// A `require` alone is harmless -- it is the `replace` that makes `go install`
// refuse the module. But a require is how a replace gets argued for later: the
// moment a production package under the root module imports the SDK, someone
// hits a version-resolution problem and reaches for a replace, and `go install
// github.com/cleat-team/cleat/cmd/cleat@vX` stops working for every tag cut
// afterwards. That failure appears only to users; nothing in CI installs the
// CLI from a version.
//
// So the invariant is not "no require", it is "no production import". This
// asserts it directly.
func TestRootModuleDoesNotDependOnSDK(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = repoRootFrom(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fatal, not Skip. `go list -deps ./...` from the repo root is always
		// satisfiable here -- case (c) in scripts/check-skips.sh's taxonomy --
		// so a failure is a broken tree or a broken workspace, not an absent
		// optional resource. Skipping would retire the one check standing
		// between this repo and a reinstated module cycle, silently, on
		// exactly the kind of breakage that causes one.
		t.Fatalf("go list -deps ./... failed, so the module cycle is unchecked:\n  %s",
			strings.TrimSpace(string(out)))
	}

	var offenders []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "github.com/cleat-team/cleat/cleat") {
			offenders = append(offenders, strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("the root module's production graph now reaches the cleat/ SDK:\n  %s\n"+
			"cleat/ requires the root module back, so this is the module cycle that "+
			"made `go install github.com/cleat-team/cleat/cmd/cleat@vX` impossible. "+
			"Move the consumer into cleat/ or into its own module, the way plugins/dag, "+
			"examples/ and tests/plugin-harness were.",
			strings.Join(offenders, "\n  "))
	}
}

// repoRootFrom locates the repo root by walking up for the go.mod that declares
// the root module. The nearest go.mod is not enough -- several directories in
// this repo are their own modules.
func repoRootFrom(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/cleat-team/cleat\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find the repo root from %s", dir)
		}
		dir = parent
	}
}
