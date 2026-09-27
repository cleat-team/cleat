package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestInitModuleDoneBranchConsultsErrCh pins the fix for a race that behavioural
// testing could not pin.
//
// InitModule waits for the guest's _start on two channels. The goroutine sends
// any failure on errCh (buffered, cap 1) and only then runs the deferred
// close(done), so when _start traps BOTH select cases are ready at once and Go
// picks uniformly at random. `case <-done: return nil` therefore discarded the
// error about half the times the poller reached that select -- reinstating, in a
// different shape, the exact bug the errCh plumbing had been added to fix.
//
// Why this is a source guard rather than a test that runs the code:
//
// Whether the poller reaches that select at all is a timing accident. On an idle
// machine the trap lands in errCh before the first 100µs backoff elapses and an
// earlier select catches it, so the defect is invisible. Measured 2026-09-01
// with the fix removed:
//
//	TestInitModuleReportsATrappingStart, default GOMAXPROCS   0 failures / 40
//	TestInitModuleReportsATrappingStart, -cpu 1               4 failures / 200
//	a 60-iteration repetition of the same call, -cpu 1        1 failure  / 30
//
// So the behavioural test is a ~2% detector, and it first surfaced in CI on an
// unrelated docs-only PR. A repetition test was written to make it reliable and
// **was not** -- it is worse than nothing, because a name like
// "...EveryTime" reads as deterministic and would be re-run until green and then
// believed. It was deleted rather than shipped.
//
// What is deterministic is the shape of the code: the branch that treats a
// closed `done` as success must consult errCh. That is what this checks.
// CLAUDE.md: proving a test can fail catches one that *cannot* fail, not one
// that fails *sometimes*.
func TestInitModuleDoneBranchConsultsErrCh(t *testing.T) {
	const path = "runtime.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Name.Name == "InitModule" && fd.Recv != nil {
			fn = fd
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatalf("no InitModule method found in %s.\n\n"+
			"If it was renamed or moved, retarget this guard. An unmatched search "+
			"makes every assertion below vacuous -- which is the failure mode this "+
			"guard exists to prevent in the first place.", path)
	}

	// Find select clauses that receive from `done`, and require each to mention
	// errCh somewhere in its body.
	var found, missing int
	ast.Inspect(fn, func(n ast.Node) bool {
		clause, ok := n.(*ast.CommClause)
		if !ok || clause.Comm == nil {
			return true
		}
		if !receivesFromIdent(clause.Comm, "done") {
			return true
		}
		found++
		if !bodyMentionsIdent(clause.Body, "errCh") {
			missing++
			t.Errorf("InitModule's `case <-done:` at %s does not consult errCh.\n\n"+
				"The goroutine sends the failure on errCh and only then closes done, "+
				"so on a trap both cases are ready and Go picks at random: this branch "+
				"returns nil for a guest whose _start failed, roughly half the times it "+
				"is reached. Drain errCh with a non-blocking receive before treating a "+
				"closed done as success.", fset.Position(clause.Pos()))
		}
		return true
	})

	// Vacuous-pass control. Zero matches would report success having checked
	// nothing, which is precisely the shape of the bug.
	if found == 0 {
		t.Fatal("found no `case <-done:` clause in InitModule.\n\n" +
			"Either the wait was restructured -- in which case re-derive what this " +
			"guard should assert, deliberately -- or the pattern match broke and this " +
			"test is now checking nothing.")
	}
	if missing == 0 && testing.Verbose() {
		t.Logf("checked %d `case <-done:` clause(s), all consult errCh", found)
	}
}

// receivesFromIdent reports whether stmt is a receive from the named channel,
// in either `case <-ch:` or `case v := <-ch:` form.
func receivesFromIdent(stmt ast.Stmt, name string) bool {
	var expr ast.Expr
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		expr = s.X
	case *ast.AssignStmt:
		if len(s.Rhs) != 1 {
			return false
		}
		expr = s.Rhs[0]
	default:
		return false
	}
	unary, ok := expr.(*ast.UnaryExpr)
	if !ok || unary.Op != token.ARROW {
		return false
	}
	ident, ok := unary.X.(*ast.Ident)
	return ok && ident.Name == name
}

func bodyMentionsIdent(body []ast.Stmt, name string) bool {
	var seen bool
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				seen = true
				return false
			}
			return true
		})
		if seen {
			return true
		}
	}
	return false
}
