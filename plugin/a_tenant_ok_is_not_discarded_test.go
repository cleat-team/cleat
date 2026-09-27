package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// tenantOkDiscardLedger lists every call to auth.TenantIDFromContext or
// auth.TenantIDFromRequest that deliberately discards ok, with the reason.
// cleat#2183.
//
// WHY THIS EXISTS. Both functions return (uuid.UUID, bool): a tenant ID and
// whether one was actually set. For 17 plugins' HTTP routes, ok was
// discarded and the UUID compared to uuid.Nil instead -- which cannot tell
// "no tenant authenticated" apart from "authenticated as the seeded default
// tenant", whose ID literally IS uuid.Nil. Every one of those routes
// rejected the default tenant's own valid API key with a 401, for as long
// as the pattern existed. The fix (auth.TenantIDFromRequest, used with ok,
// at every one of those call sites) is mechanical and repeatable; so is a
// regression of it, which is what this guards against.
//
// EXPECTED TO BE NEARLY EMPTY. One entry today: auditlog's request logging,
// which genuinely has no use for ok -- see its own comment.
var tenantOkDiscardLedger = map[string]bool{
	// cleat#1881: audit logging records an empty user_id/tenant_id for an
	// unauthenticated request rather than refusing to log it -- there is no
	// tenant to distinguish "none" from "default" for, so discarding ok
	// here is correct, not the bug this ledger exists to catch.
	"plugins/auditlog/middleware.go:(*Plugin).Middleware": true,
}

// tenantOkDiscardSite is one discarded-ok call, located by go/ast -- same
// reasoning as crossTenantSite and its siblings: a regex cannot tell a call
// from a comment describing one, and a parser does not need to try.
type tenantOkDiscardSite struct {
	File string // repo-relative
	Func string // enclosing function, "(*Recv).Name" for a method
	Line int
}

func (s tenantOkDiscardSite) key() string { return s.File + ":" + s.Func }

// isTenantOkDiscard matches `x, _ := auth.TenantIDFromContext(...)` and the
// TenantIDFromRequest / `=` shaped equivalents: a 2-target assignment whose
// second target is the blank identifier and whose right-hand side is a
// single call to a function of either name, any selector prefix -- there is
// only one such pair in the tree today (auth.TenantIDFromContext,
// auth.TenantIDFromRequest), so matching on the method name alone is exact
// without needing type information, the same tradeoff isSecretsForTenant
// makes for ForTenant.
func isTenantOkDiscard(stmt *ast.AssignStmt) bool {
	if len(stmt.Lhs) != 2 || len(stmt.Rhs) != 1 {
		return false
	}
	blank, ok := stmt.Lhs[1].(*ast.Ident)
	if !ok || blank.Name != "_" {
		return false
	}
	call, ok := stmt.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == "TenantIDFromContext" || fun.Sel.Name == "TenantIDFromRequest"
	case *ast.Ident:
		return fun.Name == "TenantIDFromContext" || fun.Name == "TenantIDFromRequest"
	}
	return false
}

// scanTenantOkDiscardSites parses each file and returns every
// isTenantOkDiscard-shaped assignment. Shares trackedGoFiles/funcName/
// repoRoot with the other ledger scanners (a_cross_tenant_bypass_is_declared_test.go,
// same package).
func scanTenantOkDiscardSites(t *testing.T, root string, files []string) []tenantOkDiscardSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []tenantOkDiscardSite

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
				stmt, ok := n.(*ast.AssignStmt)
				if !ok || !isTenantOkDiscard(stmt) {
					return true
				}
				sites = append(sites, tenantOkDiscardSite{
					File: rel,
					Func: name,
					Line: fset.Position(stmt.Pos()).Line,
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

// TestNoTenantOkIsDiscardedWithoutDeclaration is tenantOkDiscardLedger's
// ratchet, checked in both directions for the same reason
// TestEveryCrossTenantBypassIsDeclared and its siblings are: an undeclared
// site is a new discard nobody reviewed -- and on this exact call, cleat#2183
// is what an undeclared one turns into -- and a stale entry is a grant
// covering something that is not there.
//
// CACHING. This test reads the tree through the repository root, outside its
// own package directory -- go test only invalidates its cache on files
// opened INSIDE that directory. Use -count=1 locally; CI already does.
func TestNoTenantOkIsDiscardedWithoutDeclaration(t *testing.T) {
	root := repoRoot(t)
	sites := scanTenantOkDiscardSites(t, root, trackedGoFiles(t, root))

	found := map[string]tenantOkDiscardSite{}
	for _, s := range sites {
		found[s.key()] = s
	}

	for key, site := range found {
		if !tenantOkDiscardLedger[key] {
			t.Errorf("undeclared discarded-ok call at %s:%d\n"+
				"  auth.TenantIDFromContext/TenantIDFromRequest's ok return was discarded here.\n"+
				"  Comparing the returned UUID to uuid.Nil instead cannot tell \"no tenant\n"+
				"  authenticated\" apart from \"authenticated as the default tenant\", whose ID\n"+
				"  IS uuid.Nil -- cleat#2183. Use ok, or add %q to tenantOkDiscardLedger with\n"+
				"  a reason if this discard is genuinely intentional.", site.File, site.Line, key)
		}
	}
	for key := range tenantOkDiscardLedger {
		if _, ok := found[key]; !ok {
			t.Errorf("stale tenantOkDiscardLedger entry %q: no discarded-ok call there.\n"+
				"  The call was removed, fixed, or its function was renamed. A ledger line\n"+
				"  that matches nothing is a grant covering something that is not there --\n"+
				"  it will silently cover the NEXT thing to take that name.", key)
		}
	}
}

// TestTheTenantOkDiscardScannerReportsAnUndeclaredSite is the known-positive.
// TestNoTenantOkIsDiscardedWithoutDeclaration answers "does it pass when the
// tree is fine?", which every broken version of it also answers yes to. This
// is the test that answers the harder question: does it REPORT a case that
// is genuinely wrong?
func TestTheTenantOkDiscardScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "tenantokdiscard", "undeclared.go")

	sites := scanTenantOkDiscardSites(t, root, []string{fixture})
	if len(sites) != 1 {
		t.Fatalf("scanner found %d site(s) in the fixture, want 1 -- it can no longer see "+
			"what it is looking for, and would report a real discarded-ok call as absent", len(sites))
	}
	if tenantOkDiscardLedger[sites[0].key()] {
		t.Fatalf("the fixture site %s is in the ledger; it must stay undeclared, "+
			"or this test proves nothing", sites[0].key())
	}
}

// TestTheTenantOkDiscardScannerIgnoresACorrectCall is the negative control:
// a call site that USES ok, the shape the fix itself produces at every one
// of the ~80 sites cleat#2183 touched, must report zero sites. Without this,
// a matcher broad enough to catch every 2-value assignment near one of these
// calls would flag the fix as indistinguishable from the bug.
func TestTheTenantOkDiscardScannerIgnoresACorrectCall(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "tenantokdiscard", "correct.go")

	sites := scanTenantOkDiscardSites(t, root, []string{fixture})
	if len(sites) != 0 {
		t.Fatalf("scanner found %d discarded-ok site(s) in a fixture that discards nothing, "+
			"want 0 -- it can no longer tell a correct call from a discarded one, which would "+
			"make every fixed call site a false positive", len(sites))
	}
}
