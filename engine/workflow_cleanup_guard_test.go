package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// The two best-effort cleanups that releaseWorkflowResources exists to own.
var bestEffortCleanups = []string{
	"ClearStickyWorker",
	"ReleaseWorkflowConcurrencyKeys",
}

// Files in this package allowed to call them directly.
//
//   - workflow_cleanup.go is the helper itself.
//   - sharded_store.go is not a cleanup site at all: its two methods route to a
//     shard and return the error to their caller, which is the opposite of
//     dropping it.
var directCleanupCallers = map[string]bool{
	"workflow_cleanup.go": true,
	"sharded_store.go":    true,
}

// parseNonTestFiles parses every non-test .go file in this package, keyed by
// base name. Both guards below walk the same set.
func parseNonTestFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the package directory")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files found; this guard would pass by measuring nothing")
	}
	return fset, files
}

// TestBestEffortCleanupGoesThroughTheHelper fails if a post-commit cleanup site
// calls ClearStickyWorker or ReleaseWorkflowConcurrencyKeys directly instead of
// going through releaseWorkflowResources.
//
// This is the condition that made the helper worth writing. The pair was copied
// to 20 sites across three dialects and the copies drifted into three different
// treatments of the returned error, so a failure to release a concurrency key —
// which parks live workflows behind a slot held by a finished one until the
// key's TTL expires — was invisible at 18 of them. Nothing except this test
// stops the 21st site from being another copy.
//
// Deliberately not asserted here: that the helper logs. A test pinning the
// message strings would fail on any rewording while still passing if the calls
// were dropped, which is the wrong end of the tradeoff. What matters is that
// every site funnels through one place; what that place does is reviewable
// because there is only one of it.
func TestBestEffortCleanupGoesThroughTheHelper(t *testing.T) {
	fset, files := parseNonTestFiles(t)

	var violations []string
	for name, file := range files {
		if directCleanupCallers[name] {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, cleanup := range bestEffortCleanups {
				if sel.Sel.Name == cleanup {
					pos := fset.Position(call.Pos())
					violations = append(violations,
						fmt.Sprintf("%s:%d calls %s", name, pos.Line, cleanup))
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("post-commit cleanup called directly instead of through "+
			"releaseWorkflowResources:\n  %s\n\n"+
			"Call releaseWorkflowResources(s.log(), s, workflowID) instead. It runs "+
			"both cleanups in the right order and logs either failure, which a bare "+
			"call or an `_ =` does not. See engine/workflow_cleanup.go for why that "+
			"matters. If a site genuinely needs the error returned rather than "+
			"logged, add its file to directCleanupCallers with the reason.",
			strings.Join(violations, "\n  "))
	}
}

// TestBestEffortCleanupHelperIsUsed fails if releaseWorkflowResources has no
// callers, which would leave the guard above passing while measuring nothing.
// Vacuous truth is the failure mode these AST guards are most prone to.
func TestBestEffortCleanupHelperIsUsed(t *testing.T) {
	_, files := parseNonTestFiles(t)

	callers := 0
	for name, file := range files {
		if name == "workflow_cleanup.go" {
			continue // the declaration, not a call
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "releaseWorkflowResources" {
				callers++
			}
			return true
		})
	}

	// 20 when this landed. Asserting a floor rather than the exact number: new
	// terminal paths should be free to add sites, and only a collapse to zero
	// means the guard above has stopped measuring anything.
	if callers == 0 {
		t.Error("releaseWorkflowResources has no callers, so " +
			"TestBestEffortCleanupGoesThroughTheHelper is vacuously true. Either the " +
			"cleanup sites were removed — in which case delete both tests and the " +
			"helper — or they were rewritten to call the store methods directly, " +
			"which is the regression this guards against.")
	}
}
