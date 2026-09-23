// cleat#1967. README.md's first code block -- the "try it" moment -- pointed
// `cleat dev --entry-point PlaceOrder` at testdata/basic, whose PlaceOrder
// makes a DurableCall to a "catalog" service nothing in this repo provides.
// The first command an evaluator runs failed unconditionally with a
// connection-refused error, and it could still fail this way: `cleat dev`
// prints valid JSON to stdout on BOTH success and failure (emitResult and
// emitError have the same shape -- see generateDevMain in dev.go), so a check
// that only asks "did some JSON come out" would pass over the exact defect
// this issue is about. "NOT MERELY THAT IT RAN" is the standard the issue
// itself states.
//
// The block also needed a clone and didn't say so: `./testdata/basic/` is a
// repo-relative path directly after a `go install ...@latest` line, which
// reads as "no clone needed". And run from a working directory outside the
// clone -- which "no clone needed" invites -- `testdata/basic` fails to build
// at all, because it depends on this repo's go.work to supply the SDK
// (go.work's own comment: the root module deliberately does not require
// github.com/cleat-team/cleat/cleat). Fixed by adding a `git clone && cd` to
// the block and pointing it at testdata/hello instead, whose Greet makes no
// DurableCall -- nothing else has to be running for the command to complete.
//
// This runs the block's OWN `cleat dev` line, extracted rather than retyped
// (the retyping trap: a test that copies the command it means to check can
// only confirm the author's understanding on the day it was written -- see
// TestEveryTemplateDocumentedCommandActuallyRuns's comment, cleat#1947, for
// the fuller argument and the five commands that drifted past a test that
// didn't do this). It runs from a REAL git clone -- cloned locally from this
// checkout rather than from github.com, so the test is about this tree's own
// README/testdata pairing rather than about network access, and so a PR that
// changes both together is checked against itself, not against whatever is
// already published.
//
// The block's own `go install github.com/cleat-team/cleat/cmd/cleat@latest`
// line is not run: it installs the latest RELEASE, not this tree's code, and
// making a PR gate depend on a live download of a version that may not even
// contain the change under test is the same dependency
// TestFullstackTemplateRunStartsAWorkflow (cleat#1969) already rejected for
// cleat-worker, for the same reason. `cleat` is built from the clone's own
// source instead. The version/release mismatch this leaves in the README
// itself is real and is tracked separately (cleat#1779); it is not this
// issue's subject.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestREADMEsFirstCommandCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a second cleat binary and clones the repo; skipped in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "go.work")); statErr != nil {
		t.Fatalf("computed repo root %s has no go.work -- wrong directory: %v", repoRoot, statErr)
	}

	block := firstBashBlock(t, filepath.Join(repoRoot, "README.md"))

	var devLine string
	for _, line := range block {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cleat dev") {
			devLine = trimmed
		}
	}
	if devLine == "" {
		t.Fatalf("README.md's first bash block has no `cleat dev` line -- README or "+
			"extractor drifted. Block:\n%s", strings.Join(block, "\n"))
	}

	// A LOCAL clone, not github.com: this test is about the pairing between
	// README.md and testdata/ IN THIS TREE, and a network clone would check
	// the published repo instead of the PR under test.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	if out, cloneErr := exec.Command("git", "clone", "--no-hardlinks", repoRoot, cloneDir).CombinedOutput(); cloneErr != nil {
		t.Fatalf("git clone: %v\n%s", cloneErr, out)
	}

	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "cleat"), "./cmd/cleat")
	build.Dir = cloneDir
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build ./cmd/cleat (in the clone): %v\n%s", buildErr, out)
	}

	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	run := exec.Command("sh", "-c", devLine)
	run.Dir = cloneDir
	run.Env = env
	out, runErr := run.CombinedOutput()
	if runErr != nil {
		t.Fatalf("%s\n%v\n%s", devLine, runErr, out)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	jsonLine := lines[len(lines)-1]
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(jsonLine), &resp); jsonErr != nil {
		t.Fatalf("%s produced a last output line that is not JSON: %v\nline: %s\nfull output:\n%s",
			devLine, jsonErr, jsonLine, out)
	}
	// The defect this issue is about: cleat dev exits 0 and prints valid JSON
	// on the failure path too (emitError, dev.go), so checking only that a
	// result parsed as JSON would still pass against the ORIGINAL PlaceOrder
	// command failing on its catalog dial.
	if resp.Error != "" {
		t.Fatalf("%s ran but did not complete: %s\nfull output:\n%s", devLine, resp.Error, out)
	}
	if len(resp.Result) == 0 {
		t.Fatalf("%s produced neither a result nor an error\nfull output:\n%s", devLine, out)
	}
}

// firstBashBlock returns the lines inside the first ```bash fenced block in a
// markdown file.
func firstBashBlock(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	inFence := false
	var block []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			if trimmed == "```bash" {
				inFence = true
			}
			continue
		}
		if trimmed == "```" {
			return block
		}
		block = append(block, line)
	}
	t.Fatalf("%s has no fenced ```bash block, or it is never closed", path)
	return nil
}
