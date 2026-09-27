package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Nobody discards the release that storeForTenant hands back.
//
// # Why a source-level scan
//
// The lease cleat#1928 added is the reason a worker no longer holds a
// connection pool for every tenant it has ever served on MySQL and SQL Server.
// Discarding it is silent in a way the leak it replaced was not: the pool is
// pinned for the life of the process, the reaper keeps ticking, the metric
// keeps reporting a successful sweep, and nothing anywhere says a tenant can
// never be released again. The failure looks exactly like the original bug,
// with the machinery that was supposed to fix it still in place.
//
// The compiler already forces every call site to ACCEPT the third value --
// that is what made the change safe to land -- but `_` accepts it too, and
// `_` is what a reader reaches for when the extra value looks like noise. This
// is the one shape the type system cannot refuse, so it is the one worth a
// test.
//
// It does not check that the release is actually CALLED; that is what
// TestExecuteWorkflow_HoldsTheTenantsLeaseForTheWholeRun and
// TestStoreForTenant_ReleasesEveryStoreItOpens do, at runtime, where it can be
// observed rather than guessed at from syntax.
func TestNoCallerDiscardsATenantStoreLease(t *testing.T) {
	root := workerRepoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "cmd/cleat-worker/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan with nothing to scan reports a clean tree, which reads as
	// "checks passed" and means "checks never started".
	if len(files) < 5 {
		t.Fatalf("git ls-files matched %d files under cmd/cleat-worker; the scan did not "+
			"see the package", len(files))
	}

	checked := 0
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 {
				return true
			}
			name, arity := calleeAndArity(assign.Rhs[0])
			var releaseAt int
			switch {
			case name == "storeForTenant" && arity == 3:
				releaseAt = 1
			case name == "storeFor" && arity == 2:
				releaseAt = 1
			default:
				return true
			}
			if len(assign.Lhs) != arity {
				return true
			}
			checked++
			if id, ok := assign.Lhs[releaseAt].(*ast.Ident); ok && id.Name == "_" {
				t.Errorf("%s:%d discards the release from %s.\n\n"+
					"That release drops the tenant's lease on its connection pool. Discarding "+
					"it pins the pool for the life of the process -- which is the leak "+
					"cleat#1928 removed, with the reaper still running and still reporting "+
					"successful sweeps. Hold it for as long as the store is used, then call it.",
					rel, fset.Position(assign.Pos()).Line, name)
			}
			return true
		})
	}

	// The floor. Every call site could be renamed or removed and this would
	// pass without inspecting anything.
	if checked < 6 {
		t.Errorf("found only %d tenant-store resolutions to check; there were 10 on "+
			"2026-09-19. A scan that matches almost nothing passes vacuously.", checked)
	}
}

// calleeAndArity reports which of the Worker's two store resolvers a call
// expression names, and how many values it yields.
//
// THE RECEIVER IS PART OF THE MATCH, because `storeFor` is not a unique name in
// this package: apiServer has one too, with the same arity of two and an
// `error` where the Worker's has a release. Matching on the method name alone
// would read `st, err := s.storeFor(r)` as a discarded lease the moment anyone
// wrote `st, _ :=` there -- a failure about the wrong method, pointing at a
// line that is correct.
func calleeAndArity(e ast.Expr) (string, int) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", 0
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", 0
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "w" {
		return "", 0
	}
	switch sel.Sel.Name {
	case "storeForTenant":
		return "storeForTenant", 3
	case "storeFor":
		return "storeFor", 2
	}
	return "", 0
}
