package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestVetAS covers IMPROVEMENT-PLAN 2.43: `cleat vet --target assemblyscript`
// could not fail.
//
// runVetAS walked the tree counting .as/.ts files, resolved node and the
// transform and discarded both, printed "Scanned N file(s)" and returned 0 --
// on every path, for every input. A workflow calling Math.random() inside a
// durable function passed, and so did a directory with no AssemblyScript in it.
//
// The assertion that matters is the exit code on a *violating* workflow.
// Asserting only that a clean project passes would have been satisfied by the
// old always-0 implementation, which is the trap this test exists to avoid.
func TestVetAS(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping AS vet test in short mode")
	}

	hasNpx := exec.Command("npx", "--version").Run() == nil
	// Same reasoning as TestASTransform: GitHub's runners ship node and npx, so
	// their absence in CI is a broken environment rather than a licence to skip.
	// Skipping there would retire the only test asserting that vet can fail.
	if !hasNpx && os.Getenv("CI") != "" {
		t.Fatal("npx is expected on CI runners; without it `cleat vet --target assemblyscript` is untested, not optional")
	}
	if !hasNpx {
		t.Skip("AS vet test requires npx")
	}

	// One project, installed once. npm install dominates the runtime, and the
	// subtests differ only in the contents of assembly/index.ts.
	dir := newASVetProject(t)

	t.Run("rejects Math.random in a durable function", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  let r: f64 = Math.random();
  if (r > 2.0) { return "{}"; }
  return "{\"status\":\"ok\"}";
}
`)
		if code := runVetAS(dir); code == 0 {
			t.Error("runVetAS returned 0 for a workflow calling Math.random() inside a " +
				"durable function; the E001 determinism check did not fail the vet")
		}
	})

	t.Run("accepts a deterministic workflow", func(t *testing.T) {
		writeASEntry(t, dir, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, input: string): string {
  return "{\"status\":\"ok\"}";
}
`)
		if code := runVetAS(dir); code != 0 {
			t.Errorf("runVetAS returned %d for a deterministic workflow, want 0", code)
		}
	})

	// The two guards below are what make a wrong invocation an error instead of
	// a pass. Under the old implementation both returned 0.
	t.Run("missing package.json is an error", func(t *testing.T) {
		empty := t.TempDir()
		if code := runVetAS(empty); code == 0 {
			t.Error("runVetAS returned 0 for a directory with no package.json")
		}
	})

	t.Run("missing entry point is an error", func(t *testing.T) {
		noEntry := t.TempDir()
		if err := os.WriteFile(filepath.Join(noEntry, "package.json"), []byte(`{"name":"x","private":true}`), 0644); err != nil {
			t.Fatal(err)
		}
		if code := runVetAS(noEntry); code == 0 {
			t.Error("runVetAS returned 0 for a project with no assembly/index.ts")
		}
	})
}

// newASVetProject creates an AssemblyScript project wired the way a real one is
// -- both @cleat/sdk and @cleat/transform installed from the checkout, since
// runVetAS resolves the transform by package name exactly as `cleat build`
// does -- and returns its directory.
func newASVetProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "assembly"), 0755); err != nil {
		t.Fatal(err)
	}

	pkgJSON := fmt.Sprintf(`{
  "name": "test-as-vet",
  "private": true,
  "devDependencies": {
    "assemblyscript": "^0.27.0"
  },
  "dependencies": {
    "@cleat/sdk": "file:%s",
    "@cleat/transform": "file:%s"
  }
}`, filepath.Join(asRepoRoot(t), "packages", "cleat-as"), transformDir(t))

	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}
	// A trivial entry so npm install has a complete project; each subtest
	// overwrites it.
	writeASEntry(t, dir, "export function noop(): void {}\n")

	cmd := exec.Command("npm", "install", "--no-audit", "--no-fund")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		// In CI the registry is expected to be reachable, so a failure here is
		// a failure. Locally an offline working copy is a genuine "nobody asked
		// for this resource" skip.
		if os.Getenv("CI") != "" {
			t.Fatalf("npm install failed in CI: %v\n%s", err, out)
		}
		t.Skipf("npm install failed and CI is unset, treating this as an offline working copy: %v\n%s", err, out)
	}
	return dir
}

func writeASEntry(t *testing.T, dir, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "assembly", "index.ts"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
}
