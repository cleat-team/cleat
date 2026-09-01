// Package homebrew holds the Homebrew formula for cleat and the guards that
// keep it internally consistent.
//
// The formula is a source build rather than a repackaging of the release
// archives, because there is no macOS cleat-worker in those archives to
// repackage: the worker needs CGO for the wasmtime backend, and .goreleaser.yml
// builds it for linux only since the release job cannot link a CGO darwin
// binary on ubuntu. See IMPROVEMENT-PLAN.md 3.54. Building from source moves
// the CGO link to the install machine, where the Xcode Command Line Tools that
// Homebrew already requires guarantee a C toolchain.
//
// A consequence of hand-maintaining the formula is that `url`, `sha256` and
// `version` can drift out of step with each other during a release bump. These
// tests catch the drift that is checkable without a network call.
package homebrew

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func formulaSource(t *testing.T) string {
	t.Helper()
	const path = "Formula/cleat.rb"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"If the formula moved, point this test at the new path rather than "+
			"deleting it -- an unreadable formula makes every assertion below "+
			"vacuous.", path, err)
	}
	return string(raw)
}

var (
	urlRe    = regexp.MustCompile(`(?m)^\s*url\s+"([^"]+)"`)
	sha256Re = regexp.MustCompile(`(?m)^\s*sha256\s+"([0-9a-f]{64})"`)
	tagInURL = regexp.MustCompile(`/tags/(v[0-9]+\.[0-9]+\.[0-9]+)\.tar\.gz$`)
)

// TestFormulaPinsATaggedSourceTarball checks the parts of a release bump that
// can be got wrong silently.
//
// A sha256 that is 64 hex characters but belongs to the previous tarball cannot
// be caught here -- that needs the network, and `brew audit` does it at release
// time. What can be caught is a url/sha256 pair where one was updated and the
// other was not noticed at all, and a url that does not name a tag.
func TestFormulaPinsATaggedSourceTarball(t *testing.T) {
	src := formulaSource(t)

	url := urlRe.FindStringSubmatch(src)
	if url == nil {
		t.Fatal("the formula declares no url. Homebrew cannot fetch anything, and " +
			"every other assertion in this file is about a formula that does not install.")
	}
	if sha := sha256Re.FindStringSubmatch(src); sha == nil {
		t.Error("the formula declares no sha256, or one that is not 64 hex characters.\n\n" +
			"Without it Homebrew cannot verify what it downloaded.")
	}

	tag := tagInURL.FindStringSubmatch(url[1])
	if tag == nil {
		t.Fatalf("url %q does not point at a tagged source tarball.\n\n"+
			"It must be .../archive/refs/tags/vX.Y.Z.tar.gz. A branch or commit URL "+
			"has no stable checksum, so the sha256 would rot on the next push.", url[1])
	}

	// The `head` line intentionally points at a branch and must not be
	// mistaken for the release URL.
	if strings.Contains(url[1], "develop") {
		t.Errorf("url %q points at a branch, not a release tag", url[1])
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
