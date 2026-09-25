package pinnedtx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The fix is only as good as its call sites. Both migration runners take a
// pinned *sql.Conn on PostgreSQL, and a BeginTx(ctx, ...) on the session they
// hold brings the hazard straight back. This reads the two files with the Go
// parser rather than grepping them: a comment that says "BeginTx(ctx" -- and
// this package has several -- is not a call.

// rawBeginTxCalls returns the position of every call of the form
// <anything>.BeginTx(...) in src whose receiver is an identifier called
// "session", which is what both runners name the pinned handle.
func rawBeginTxCalls(t *testing.T, filename string, src any) (bad []token.Position, pinnedCalls int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			switch {
			case id.Name == "session" && sel.Sel.Name == "BeginTx":
				bad = append(bad, fset.Position(call.Pos()))
			case id.Name == "pinnedtx" && sel.Sel.Name == "Begin":
				pinnedCalls++
			}
		}
		return true
	})
	return bad, pinnedCalls
}

func TestNoMigrationBeginsATransactionOnItsSessionWithTheRunContext(t *testing.T) {
	// Known-positive: the scan must flag the pattern it exists to forbid, and
	// must not flag the fixed one.
	bad, _ := rawBeginTxCalls(t, "positive.go", "package p\nfunc f(session S, ctx C) { session.BeginTx(ctx, nil) }\n")
	if len(bad) != 1 {
		t.Fatalf("the scan does not see a raw session.BeginTx call (%d found); it would pass a tree that has one", len(bad))
	}
	bad, pinned := rawBeginTxCalls(t, "negative.go", "package p\nfunc f(session S, ctx C) { pinnedtx.Begin(ctx, session, nil) }\n")
	if len(bad) != 0 || pinned != 1 {
		t.Fatalf("the scan misreads the fixed form: bad=%d pinned=%d", len(bad), pinned)
	}

	// Every non-test file of both packages, not just the two that call it
	// today: plugin/migration_down.go arrived after the fix and a transaction
	// added there would bring the hazard back unnoticed.
	var files []string
	for _, pattern := range []string{
		filepath.Join("..", "..", "migration", "*.go"),
		filepath.Join("..", "..", "plugin", "migration*.go"),
	} {
		m, err := filepath.Glob(pattern)
		if err != nil || len(m) == 0 {
			t.Fatalf("no files match %s (%v)", pattern, err)
		}
		files = append(files, m...)
	}
	totalPinned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		bad, pinned := rawBeginTxCalls(t, file, nil)
		totalPinned += pinned
		for _, pos := range bad {
			t.Errorf("%s: session.BeginTx on the run's context; use pinnedtx.Begin (cleat#2215)", pos)
		}
	}
	if totalPinned < 2 {
		t.Errorf("found %d pinnedtx.Begin calls across the migration files, want at least 2 (the runner and the plugin migrations) -- "+
			"either the fix was removed or this scan no longer reads them", totalPinned)
	}
}
