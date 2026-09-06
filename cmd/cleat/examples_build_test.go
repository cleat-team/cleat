package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestEveryGoExampleBuilds compiles every Go example workflow with
// `cleat build`, which nothing did before 2026-09-06.
//
// IMPROVEMENT-PLAN 3.228 measured the cost of that gap: SIX of the eight
// example workflows did not build. Three returned a struct pointer from an
// entry point, one called time.Now() inside a workflow, and two were rejected
// by threading-check defects (3.229, 3.230) that also made auto-threading
// unreachable and its output uncompilable.
//
// None of it was visible to CI, and the reason is worth stating: the examples
// are VALID GO. `cd examples && go build ./... && go vet ./...` is clean, so
// every Go job passed. The only CI reference to examples/ was
// examples/as-workflow, which is AssemblyScript. The one command the examples
// document -- `cleat build -o /tmp/out ./examples/<name>/` -- was the one
// nobody ran.
//
// Each example gets its OWN output directory. Sharing one is not a shortcut
// that works: `cleat build` copies source into the build directory and does not
// clear it, so a shared directory makes every build after the first inherit the
// previous example's files. That produced errors naming files from other
// examples and, in the sweep that found 3.228, nine failures out of nine -- a
// number that was wrong in the direction that made the finding look bigger.
func TestEveryGoExampleBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain on PATH, yet `go test` is executing this: %v", err)
	}

	root := filepath.Join("..", "..")
	entries, err := os.ReadDir(filepath.Join(root, "examples"))
	if err != nil {
		t.Fatalf("reading examples/: %v", err)
	}

	// Which directories are workflows is decided by BUILDING them, not by a
	// regex over the source. There is no //cleat:entry marker to look for --
	// IsEntryPoint (internal/analyzer/loader.go) says an entry point is an
	// exported non-method function whose first parameter is cleat.HostCalls --
	// and a pattern guessing at that would be one more text search applied to a
	// format it does not model.
	//
	// So: build everything with Go files, and treat the tool's own
	// "no workflow entry points found" as "not a workflow". examples/
	// third-party-plugin is the one such directory today.
	const notAWorkflow = "no workflow entry points found"

	var built, skipped []string
	var mu sync.Mutex

	var dirs []string
	for _, e := range entries {
		if e.IsDir() && hasGoFiles(filepath.Join(root, "examples", e.Name())) {
			dirs = append(dirs, e.Name())
		}
	}

	for _, name := range dirs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			src := filepath.Join(root, "examples", name)
			cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, src)
			out, err := cmd.CombinedOutput()

			if err != nil && strings.Contains(string(out), notAWorkflow) {
				mu.Lock()
				skipped = append(skipped, name)
				mu.Unlock()
				t.Logf("not a workflow (no entry points), so not a build target: %s", name)
				return
			}
			if err != nil {
				t.Fatalf("cleat build failed on a shipped example:\n%s\n%v", out, err)
			}
			mu.Lock()
			built = append(built, name)
			mu.Unlock()
		})
	}

	t.Cleanup(func() {
		// A floor, because "every example built" is also what a run that built
		// NOTHING reports. Eight workflows exist today; five is a deliberately
		// loose floor that still catches a collapse.
		if len(built) < 5 {
			t.Errorf("only %d example workflows were built (%v); skipped as non-workflows: %v.\n"+
				"This test passes vacuously if it stops finding them, which is the failure "+
				"mode it is guarding against.", len(built), built, skipped)
		}
	})
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
