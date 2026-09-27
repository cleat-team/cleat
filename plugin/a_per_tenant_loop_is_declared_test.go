package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// perTenantLoopLedger lists every plugin.AllTenantIDs call site, keyed the
// same way crossTenantLedger is: "<repo-relative file>:<enclosing function>".
//
// WHY THIS EXISTS, SEPARATELY FROM crossTenantLedger. Listing admin.tenants
// carries no elevated access by itself -- it is not a new privilege the way
// AcrossAllTenants is. But plugin.AllTenantIDs plus plugin.ForTenant per id is
// a SECOND way for a background loop to act on every tenant, and cleat#2141
// introduced the idiom (plugins/jobqueue's sweepAbandonedJobsPerTenant,
// plugins/blobstore's allInFlightWorkflowIDsMSSQL). A reviewer who greps only
// crossTenantLedger for "does this plugin touch every tenant" gets a false
// negative on a loop-shaped answer to the same question -- exactly the gap
// TestEveryCrossTenantBypassIsDeclared exists to close for the bypass shape.
// This is that guard's sibling, for the loop shape.
var perTenantLoopLedger = map[string]bool{
	"plugins/jobqueue/background.go:(*Plugin).sweepAbandonedJobsPerTenant":  true,
	"plugins/blobstore/background.go:(*Plugin).allInFlightWorkflowIDsMSSQL": true,
}

// perTenantLoopSite is one plugin.AllTenantIDs call, located by go/ast --
// same reasoning as crossTenantSite: a regex cannot tell a call from a
// comment describing one, and a parser does not need to try.
type perTenantLoopSite struct {
	File string // repo-relative
	Func string // enclosing function, "(*Recv).Name" for a method
	Line int
}

func (s perTenantLoopSite) key() string { return s.File + ":" + s.Func }

// isAllTenantIDs matches both spellings: plugin.AllTenantIDs from outside the
// package, and the bare name from within it.
func isAllTenantIDs(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "plugin" && e.Sel.Name == "AllTenantIDs"
	case *ast.Ident:
		return e.Name == "AllTenantIDs"
	}
	return false
}

// scanPerTenantLoopSites parses each file and returns every AllTenantIDs
// call. Shares trackedGoFiles/funcName/repoRoot with the cross-tenant
// scanner (a_cross_tenant_bypass_is_declared_test.go, same package).
func scanPerTenantLoopSites(t *testing.T, root string, files []string) []perTenantLoopSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []perTenantLoopSite

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := funcName(fn)
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isAllTenantIDs(call.Fun) {
					return true
				}
				sites = append(sites, perTenantLoopSite{
					File: rel,
					Func: name,
					Line: fset.Position(call.Pos()).Line,
				})
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}

// TestEveryPerTenantLoopIsDeclared is perTenantLoopLedger's ratchet, checked
// in both directions for the same reason TestEveryCrossTenantBypassIsDeclared
// is: an undeclared site is a new per-tenant loop nobody reviewed, and a
// stale entry is a grant covering something that is not there.
func TestEveryPerTenantLoopIsDeclared(t *testing.T) {
	root := repoRoot(t)
	sites := scanPerTenantLoopSites(t, root, trackedGoFiles(t, root))

	found := map[string]perTenantLoopSite{}
	for _, s := range sites {
		found[s.key()] = s
	}

	for key, site := range found {
		if !perTenantLoopLedger[key] {
			t.Errorf("undeclared per-tenant loop at %s:%d\n"+
				"  plugin.AllTenantIDs plus plugin.ForTenant per id is a second way to act on\n"+
				"  every tenant, alongside AcrossAllTenants. Add %q to perTenantLoopLedger, or\n"+
				"  thread the tenant through instead if one is actually available on this path --\n"+
				"  a loop that did not need to exist is the failure this ledger is for.",
				site.File, site.Line, key)
		}
	}
	for key := range perTenantLoopLedger {
		if _, ok := found[key]; !ok {
			t.Errorf("stale per-tenant loop ledger entry %q: no plugin.AllTenantIDs call there.\n"+
				"  The call was removed or its function was renamed. A ledger line that matches\n"+
				"  nothing is a grant covering something that is not there -- it will silently\n"+
				"  cover the NEXT thing to take that name.", key)
		}
	}
	if len(found) == 0 {
		t.Fatal("the scanner found no plugin.AllTenantIDs call sites at all, which cannot be " +
			"right while the ledger is non-empty -- the file list or the matcher is broken, " +
			"and a guard that looks at nothing reports everything as fine")
	}
}

// TestThePerTenantLoopScannerReportsAnUndeclaredSite is the known-positive.
// TestEveryPerTenantLoopIsDeclared answers "does it pass when the tree is
// fine?", which every broken version of it also answers yes to. This answers
// the harder question: does it REPORT a case that is genuinely wrong?
func TestThePerTenantLoopScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "pertenant", "undeclared_loop.go")

	sites := scanPerTenantLoopSites(t, root, []string{fixture})
	if len(sites) != 1 {
		t.Fatalf("scanner found %d site(s) in the fixture, want 1 -- it can no longer see "+
			"what it is looking for, and would report a real undeclared per-tenant loop as absent", len(sites))
	}
	if perTenantLoopLedger[sites[0].key()] {
		t.Fatalf("the fixture site %s is in the ledger; it must stay undeclared, "+
			"or this test proves nothing", sites[0].key())
	}
}
