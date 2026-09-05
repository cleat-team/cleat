package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryMSSQLTransactionBoundaryIsRetried asserts that every function in the
// SQL Server store that opens a transaction is reached through
// withRollbackGuaranteedRetry.
//
// A deadlock victim (1205) or a snapshot conflict (3960) is rolled back by the
// server, so the caller sees an ordinary error and the work is simply lost.
// IMPROVEMENT-PLAN.md 2.26 wrapped the boundaries one file at a time and
// deferred mssql_events.go and mssql_signals_promises.go, because 2.60 was
// rewriting them. 2.60 landed as #283 on 2026-08-04; the deferral outlived it
// by a month, which is what this test exists to stop happening again.
//
// A set-membership guard would not have caught that: the deferred boundaries
// were never in a baseline to begin with. This asserts the property directly,
// so a new BeginTx in any mssql_*.go file fails until it is wrapped.
//
// Deliberately structural rather than behavioural. Proving a retry happens
// needs a live deadlock against a real SQL Server, which
// engine/mssql_deadlock_test.go does for the paths it covers and which skips
// wherever CLEAT_TEST_MSSQL is unset -- so it cannot be the thing that stops a
// boundary being added unwrapped.
func TestEveryMSSQLTransactionBoundaryIsRetried(t *testing.T) {
	files, err := filepath.Glob("mssql_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no mssql_*.go files found -- this test is scanning nothing, " +
			"which would pass vacuously")
	}

	fset := token.NewFileSet()
	opensTx := map[string]string{} // func name -> file
	retried := map[string]bool{}   // func name mentioned inside a retry closure
	scanned := 0

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncDecl:
				if v.Body == nil {
					return true
				}
				// A boundary opens a transaction *and* commits it. A helper
				// that opens one and hands it back -- beginTxWithContext -- is
				// not a boundary; its callers are, and they are checked on
				// their own. tenantSessionConn.BeginTx is a driver passthrough
				// and commits nothing either. Both showed up when this test
				// keyed on BeginTx alone.
				if callsSelector(v.Body, "BeginTx") && callsSelector(v.Body, "Commit") {
					opensTx[v.Name.Name] = f
				}
			case *ast.CallExpr:
				id, ok := v.Fun.(*ast.Ident)
				if !ok || id.Name != "withRollbackGuaranteedRetry" {
					return true
				}
				// Any function named anywhere inside the retry closure counts as
				// reached. The closure is the whole point of the wrapper, so a
				// name appearing there is being retried.
				for _, arg := range v.Args {
					ast.Inspect(arg, func(m ast.Node) bool {
						if sel, ok := m.(*ast.SelectorExpr); ok {
							retried[sel.Sel.Name] = true
						}
						return true
					})
				}
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test mssql files")
	}
	if len(opensTx) == 0 {
		t.Fatalf("found no BeginTx in %d mssql files -- the scan is broken, not the tree", scanned)
	}

	var unwrapped []string
	for fn, file := range opensTx {
		if !retried[fn] {
			unwrapped = append(unwrapped, fn+"  ("+file+")")
		}
	}
	sort.Strings(unwrapped)
	for _, u := range unwrapped {
		t.Errorf("opens a transaction and is never reached through "+
			"withRollbackGuaranteedRetry: %s\n\tA deadlock victim on this path is "+
			"rolled back by the server and surfaces as an ordinary error, so the "+
			"work is lost rather than replayed. Wrap it the way CompactHistory "+
			"does, splitting the body into a %sOnce.", u, strings.Fields(u)[0])
	}
}

// callsSelector reports whether body contains a call whose selector is name.
func callsSelector(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}
