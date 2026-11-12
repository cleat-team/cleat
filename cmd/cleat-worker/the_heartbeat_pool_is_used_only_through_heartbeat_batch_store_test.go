package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// cleat#3571. Another tenant's beat now goes through the isolated heartbeat pool
// re-scoped to that tenant (heartbeatStoreForTenant). On SQL Server a beat sets
// its tenant's session context on a pooled connection, and measured on the
// shipped driver that context is gone when the connection is next used (and is
// visible on it until then). HeartbeatBatchFenced does not depend on that: it
// asserts its tenant at the start of every transaction. A plain statement run on
// this pool would, so the pool is for HeartbeatBatchFenced and nothing else.
//
// So this holds the POOL, not only the field, to HeartbeatBatchFenced. It parses
// the package rather than grepping it, so a comment that mentions a name is not a
// use and a use inside a closure is not missed, and it asks three things:
//
//  1. Worker.heartbeatStore is read only by heartbeatBatchStore and
//     heartbeatStoreForTenant.
//  2. Each of those two accessors is CALLED from exactly one place, and that place
//     is heartbeatAndFenceInFlight. A second caller is a second user of the pool.
//  3. In that function, the variable the accessors' results are assigned to is
//     only ever used as the receiver of HeartbeatBatchFenced.
//
// Each was checked with a mutant: a second reader of the field, a second caller of
// an accessor, and a second method called on the result.
func TestTheHeartbeatPoolIsUsedOnlyThroughHeartbeatBatchStore(t *testing.T) {
	const user = "heartbeatAndFenceInFlight"
	accessors := map[string]bool{
		"heartbeatBatchStore":     true,
		"heartbeatStoreForTenant": true,
	}
	// git ls-files rather than ParseDir (deprecated, SA1019) or a walk: a walk
	// descends into .claude/worktrees/, whole copies of the repository, and would
	// attribute a scratch checkout's code to this package.
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range trackedGoFiles(t) {
		if strings.Contains(path, "/") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no tracked non-test files in this package: the scan would pass vacuously")
	}
	var problems []string
	fieldReaders := map[string]bool{}
	callSites := map[string][]string{}
	var resultVars map[string]bool // identifiers the accessors' results are assigned to, in `user`
	var userDecl *ast.FuncDecl
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == user {
				userDecl = fn
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "heartbeatStore" {
						if accessors[fn.Name.Name] {
							fieldReaders[fn.Name.Name] = true
						} else {
							problems = append(problems, fset.Position(x.Pos()).String()+" reads Worker.heartbeatStore in "+fn.Name.Name)
						}
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && accessors[sel.Sel.Name] {
						callSites[sel.Sel.Name] = append(callSites[sel.Sel.Name], fset.Position(x.Pos()).String()+" in "+fn.Name.Name)
						if fn.Name.Name != user {
							problems = append(problems, fset.Position(x.Pos()).String()+" calls "+sel.Sel.Name+" in "+fn.Name.Name+", not in "+user)
						}
					}
				}
				return true
			})
		}
	}
	for name := range accessors {
		if !fieldReaders[name] {
			problems = append(problems, "the scan never saw "+name+" read heartbeatStore: it is not looking at what it claims to")
		}
		if got := callSites[name]; len(got) != 1 {
			problems = append(problems, fmt.Sprintf("%s is called from %d place(s) %v, want exactly 1 (in %s)", name, len(got), got, user))
		}
	}
	if userDecl == nil {
		t.Fatalf("%s was not found", user)
	}

	// 3. What the accessors' results are assigned to, inside `user`.
	resultVars = map[string]bool{}
	ast.Inspect(userDecl, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range as.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && accessors[sel.Sel.Name] {
				// `a, b := w.heartbeatBatchStore(), func() {}` pairs by position;
				// `a, b, c = w.heartbeatStoreForTenant(g)` assigns the first.
				if len(as.Lhs) == len(as.Rhs) {
					for i := range as.Rhs {
						if as.Rhs[i] == rhs {
							if id, ok := as.Lhs[i].(*ast.Ident); ok {
								resultVars[id.Name] = true
							}
						}
					}
				} else if id, ok := as.Lhs[0].(*ast.Ident); ok {
					resultVars[id.Name] = true
				}
			}
		}
		return true
	})
	if len(resultVars) != 1 {
		problems = append(problems, fmt.Sprintf("the accessors' results go to %d variable(s) %v in %s, want exactly 1", len(resultVars), resultVars, user))
	}
	ast.Inspect(userDecl, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && resultVars[id.Name] && sel.Sel.Name != "HeartbeatBatchFenced" {
			problems = append(problems, fset.Position(sel.Pos()).String()+" calls "+sel.Sel.Name+" on the heartbeat pool's store")
		}
		return true
	})
	// Every other use of the variable must be an assignment target, which is where it is set.
	assignTargets := map[*ast.Ident]bool{}
	ast.Inspect(userDecl, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					assignTargets[id] = true
				}
			}
		}
		return true
	})
	receivers := map[*ast.Ident]bool{}
	ast.Inspect(userDecl, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				receivers[id] = true
			}
		}
		return true
	})
	ast.Inspect(userDecl, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && resultVars[id.Name] && !assignTargets[id] && !receivers[id] {
			problems = append(problems, fset.Position(id.Pos()).String()+" uses "+id.Name+" other than as a receiver: the pool's store escapes")
		}
		return true
	})

	sort.Strings(problems)
	for _, p := range problems {
		t.Errorf("%s. The heartbeat pool carries other tenants' beats re-scoped with WithTenant, and on SQL Server a beat "+
			"leaves its tenant's session context on the pooled connection until it is next used; anything but "+
			"HeartbeatBatchFenced run on it would read that tenant's rows (cleat#3571)", p)
	}
}
