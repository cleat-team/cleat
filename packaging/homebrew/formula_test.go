// Package homebrew holds the Homebrew formula TEMPLATE for cleat and the
// guards that keep it internally consistent.
//
// The formula is a source build rather than a repackaging of the release
// archives, because there is no macOS cleat-worker in those archives to
// repackage: the worker needs CGO for the wasmtime backend, and .goreleaser.yml
// builds it for linux only since the release job cannot link a CGO darwin
// binary on ubuntu. See IMPROVEMENT-PLAN.md 3.54. Building from source moves
// the CGO link to the install machine, where the Xcode Command Line Tools that
// Homebrew already requires guarantee a C toolchain.
//
// cleat#2068: Formula/cleat.rb.tmpl is not itself installable -- its url and
// sha256 are placeholder tokens. The installable formula lives at
// cleat-team/homebrew-tap, generated from this template by
// scripts/render-homebrew-formula.sh, and pushed there by
// .github/workflows/release.yml's homebrew-bump job after each GitHub
// Release. That is what replaced the old hand-maintained bump (see
// docs/project/release-process.md) and what makes a stale url/sha256 pair
// structurally impossible here: this file never carries real values to get
// out of step.
package homebrew

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func formulaSource(t *testing.T) string {
	t.Helper()
	const path = "Formula/cleat.rb.tmpl"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"If the template moved, point this test at the new path rather than "+
			"deleting it -- an unreadable template makes every assertion below "+
			"vacuous.", path, err)
	}
	return string(raw)
}

var (
	urlRe    = regexp.MustCompile(`(?m)^\s*url\s+"([^"]+)"`)
	sha256Re = regexp.MustCompile(`(?m)^\s*sha256\s+"([^"]+)"`)
)

// TestFormulaTemplatePlaceholdersAreWellFormed checks the parts of the
// render substitution that can be got wrong silently.
//
// This template carries no real url or sha256 -- see the package doc --
// so what it can check is narrower than before cleat#2068: that the two
// placeholder tokens scripts/render-homebrew-formula.sh depends on are
// exactly where that script expects them. A typo in either token (here or
// in the script) would make the substitution silently match nothing, which
// TestRenderProducesAPinnedTaggedFormula below is what actually catches --
// this test only confirms the template's half of that contract.
func TestFormulaTemplatePlaceholdersAreWellFormed(t *testing.T) {
	src := formulaSource(t)

	url := urlRe.FindStringSubmatch(src)
	if url == nil {
		t.Fatal("the template declares no url. Rendering cannot produce an " +
			"installable formula, and every other assertion in this file is " +
			"about one that does not install.")
	}
	if url[1] != "https://github.com/cleat-team/cleat/archive/refs/tags/__CLEAT_TAG__.tar.gz" {
		t.Errorf("url %q does not carry the __CLEAT_TAG__ placeholder in the "+
			"expected shape.\n\nscripts/render-homebrew-formula.sh substitutes "+
			"__CLEAT_TAG__ literally into this line; a hand-edited or reshaped "+
			"url here would make that substitution a no-op that still exits 0.", url[1])
	}

	sha := sha256Re.FindStringSubmatch(src)
	if sha == nil {
		t.Fatal("the template declares no sha256 field at all.")
	}
	if sha[1] != "__CLEAT_SHA256__" {
		t.Errorf("sha256 %q is not the __CLEAT_SHA256__ placeholder.\n\n"+
			"A real-looking 64-hex value here would be silently stale forever -- "+
			"this template is never installed directly, so nothing would ever "+
			"notice it stopped matching a real tarball.", sha[1])
	}

	// The `head` line intentionally points at a branch and must never be
	// mistaken for the release URL.
	if strings.Contains(url[1], "develop") {
		t.Errorf("url %q points at a branch, not a release tag", url[1])
	}
}

// TestRenderProducesAPinnedTaggedFormula is the known-positive dry run for
// cleat#2068: it runs the actual script the release job runs, against a
// fake tag and a dummy sha256, and checks the OUTPUT -- not just that the
// script exits 0. A script that silently failed to substitute (say, because
// the template's placeholder text drifted from what the script's sed
// expects) would still exit 0 and produce a formula that is byte-identical
// to the template, which is precisely what this test would catch and a
// bare exit-code check would not.
func TestRenderProducesAPinnedTaggedFormula(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// The script resolves its default template path relative to the repo
	// root (packaging/homebrew/Formula/cleat.rb.tmpl), so it must run from
	// there, not from this package's directory.
	repoRoot := strings.TrimSuffix(root, "/packaging/homebrew")
	if repoRoot == root {
		t.Fatalf("expected to be running from .../packaging/homebrew, got %q", root)
	}

	const fakeTag = "v9.9.9"
	const fakeSHA = "1111111111111111111111111111111111111111111111111111111111111111" // 64 hex chars

	cmd := exec.Command("scripts/render-homebrew-formula.sh", fakeTag, fakeSHA)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render-homebrew-formula.sh %s %s: %v\n\n%s", fakeTag, fakeSHA, err, out)
	}
	rendered := string(out)

	if strings.Contains(rendered, "__CLEAT_TAG__") || strings.Contains(rendered, "__CLEAT_SHA256__") {
		t.Fatalf("rendered output still contains a placeholder token:\n%s", rendered)
	}

	wantURL := "url \"https://github.com/cleat-team/cleat/archive/refs/tags/" + fakeTag + ".tar.gz\""
	if !strings.Contains(rendered, wantURL) {
		t.Errorf("rendered output does not contain %q", wantURL)
	}
	wantSHA := "sha256 \"" + fakeSHA + "\""
	if !strings.Contains(rendered, wantSHA) {
		t.Errorf("rendered output does not contain %q", wantSHA)
	}

	// The render must not corrupt the structural parts the tests below
	// check on the template -- confirm they survive on the rendered output
	// too, since that is what actually gets pushed to the tap.
	if !strings.Contains(rendered, `CGO_ENABLED: "1"`) {
		t.Error("rendered formula lost its CGO_ENABLED=1 block")
	}
	if !strings.Contains(rendered, "verify-backend: OK") {
		t.Error("rendered formula lost its --verify-backend assertion")
	}
}

// TestRenderRejectsAMalformedTagOrSHA is the negative control for the test
// above: the render script must refuse, not silently produce a broken
// formula, when given input that cannot be a real release.
func TestRenderRejectsAMalformedTagOrSHA(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	repoRoot := strings.TrimSuffix(root, "/packaging/homebrew")

	const validSHA = "1111111111111111111111111111111111111111111111111111111111111111" // 64 hex chars

	cases := []struct {
		name string
		tag  string
		sha  string
	}{
		{"tag missing leading v", "9.9.9", validSHA},
		{"tag names a branch", "develop", validSHA},
		{"sha too short", "v9.9.9", "deadbeef"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("scripts/render-homebrew-formula.sh", c.tag, c.sha)
			cmd.Dir = repoRoot
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("render-homebrew-formula.sh %s %s: expected a non-zero exit, got 0\n%s",
					c.tag, c.sha, out)
			}
		})
	}
}

// TestFormulaBuildsTheWorkerWithCGO is the assertion the whole formula exists
// for.
//
// A cleat-worker built without CGO cannot construct the wasmtime backend --
// wasmtime is the only WASM backend cleat has -- and exits 1 during startup
// before it reads a flag. That is precisely the binary the release archives
// shipped until IMPROVEMENT-PLAN.md 3.54, and a Homebrew formula that omitted
// CGO_ENABLED=1 would reproduce it on macOS while looking like a fix.
func TestFormulaBuildsTheWorkerWithCGO(t *testing.T) {
	src := formulaSource(t)

	if !strings.Contains(src, `CGO_ENABLED: "1"`) {
		t.Error("the formula never sets CGO_ENABLED=1.\n\n" +
			"cleat-worker built without CGO exits 1 at startup: wasmtime is the only " +
			"WASM backend, and the no-cgo stub returns ErrWasmtimeCGOUnavailable. " +
			"Installing that on macOS would look like a fix and deliver a worker that " +
			"does not run.")
	}

	// The worker must be inside the CGO_ENABLED=1 block, not merely mentioned
	// somewhere in the file. Take the text from the CGO=1 marker to the next
	// with_env and require the worker build to appear in it.
	i := strings.Index(src, `CGO_ENABLED: "1"`)
	rest := src[i:]
	if j := strings.Index(rest, "with_env(CGO_ENABLED: \"0\")"); j >= 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "./cmd/cleat-worker") {
		t.Error("cleat-worker is not built inside the CGO_ENABLED=1 block.\n\n" +
			"The formula sets CGO_ENABLED=1 somewhere, but not around the build that " +
			"needs it, which is the same defect wearing a different shape.")
	}
}

// TestFormulaTestBlockExecutesTheWorker guards the guard.
//
// Setting CGO_ENABLED=1 is a claim about a build flag. Whether the resulting
// binary can actually construct the backend is a different question -- a
// broken wasmtime-go release or a toolchain problem would satisfy the
// assertion above and still install a worker that exits 1.
//
// `brew test` runs the test block, and --verify-backend constructs the backend
// for real, so this is what turns the flag into evidence.
func TestFormulaTestBlockExecutesTheWorker(t *testing.T) {
	src := formulaSource(t)

	i := strings.Index(src, "test do")
	if i < 0 {
		t.Fatal("the formula has no test block.\n\n" +
			"Nothing then executes the installed worker, and a formula that builds " +
			"a dead binary installs cleanly.")
	}
	block := src[i:]

	if !strings.Contains(block, "--verify-backend") {
		t.Error("the test block never runs cleat-worker --verify-backend.\n\n" +
			"That is the only step that constructs the wasmtime backend for real. " +
			"Without it the formula can install a worker that exits 1 at startup.")
	}
	if !strings.Contains(block, "verify-backend: OK") {
		t.Error("the test block does not assert on --verify-backend's output.\n\n" +
			"Exit status alone is weaker than it looks here: assert the OK line so " +
			"the check cannot pass by matching some other command's success.")
	}
}
