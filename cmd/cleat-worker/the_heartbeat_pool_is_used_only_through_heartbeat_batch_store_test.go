package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// So this holds the field to its two readers. It parses the package rather than
// grepping it, so a comment that mentions the field is not a use and a use inside
// a closure is not missed.
func TestTheHeartbeatPoolIsUsedOnlyThroughHeartbeatBatchStore(t *testing.T) {
	allowed := map[string]bool{
		"heartbeatBatchStore":     true,
		"heartbeatStoreForTenant": true,
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	var uses []string
	found := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "heartbeatStore" {
						return true
					}
					if allowed[fn.Name.Name] {
						found[fn.Name.Name] = true
						return true
					}
					uses = append(uses, fset.Position(sel.Pos()).String()+" in "+fn.Name.Name)
					return true
				})
			}
		}
	}
	sort.Strings(uses)
	for _, u := range uses {
		t.Errorf("%s reads Worker.heartbeatStore. The heartbeat pool carries other tenants' beats re-scoped with "+
			"WithTenant, and on SQL Server a beat leaves its tenant's session context on the pooled connection until it is "+
			"next used; anything but HeartbeatBatchFenced run on it would read that tenant's rows. Go through "+
			"heartbeatBatchStore, or give it a pool of its own (cleat#3571)", u)
	}
	// The scan must have seen the readers it exists to allow, or an empty result
	// is a scan that matched nothing.
	for name := range allowed {
		if !found[name] {
			t.Errorf("the scan never saw %s read heartbeatStore: it is not looking at what it claims to", name)
		}
	}
}
