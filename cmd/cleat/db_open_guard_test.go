package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is the guard for the defect openPostgresDB (db.go) was introduced to
// fix. Ten call sites across main.go and plugin_cmd.go each called
// sql.Open("postgres", connStr) directly, so a DSN for any other dialect
// reached lib/pq unfiltered and failed with whatever lib/pq made of a string
// it was never built to parse -- see db.go's doc comment for the exact
// messages measured on 2026-08-09. Fixed by routing every DB-opening path
// through one helper that rejects a non-Postgres DSN with an actionable
// error.
//
// Ten call sites is nine too many places to remember a check. This test is
// the mechanism, not the sweep: it fails the build the moment a new
// subcommand calls sql.Open directly instead of going through the helper,
// rather than relying on the next author to have read this comment. It
// needs no database and runs in every job.
//
// Deliberately excludes db.go itself, which is the one file allowed to call
// sql.Open -- and only inside openPostgresDB.
func TestNoDirectSQLOpenOutsideHelper(t *testing.T) {
	const allowedFile = "db.go"
	const allowedFunc = "openPostgresDB"

	fset := token.NewFileSet()
	var findings []string

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}

		base := filepath.Base(path)

		// Walk function declarations so we can tell the AST inspector which
		// function (if any) a call site sits inside, and permit sql.Open only
		// inside db.go's openPostgresDB.
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if base == allowedFile && fn.Name.Name == allowedFunc {
				// This is the one permitted call site; don't descend looking
				// for violations inside it.
				return false
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkgIdent.Name == "sql" && sel.Sel.Name == "Open" {
					findings = append(findings, path+": sql.Open called outside openPostgresDB in "+fn.Name.Name)
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/cleat: %v", err)
	}

	if len(findings) > 0 {
		t.Fatalf("cmd/cleat must call sql.Open only from openPostgresDB in db.go, so every DB-opening "+
			"path gets the dialect check. Found direct sql.Open call(s) outside it:\n%s\n"+
			"Route through openPostgresDB(connStr) instead.",
			strings.Join(findings, "\n"))
	}
}
