package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// secretsForTenantLedger lists every Secrets.ForTenant / TenantSecrets-typed
// call site, keyed the same way crossTenantLedger and perTenantLoopLedger
// are: "<repo-relative file>:<enclosing function>".
//
// WHY THIS EXISTS, SEPARATELY FROM crossTenantLedger AND perTenantLoopLedger.
// cleat#1992's plugin.Secrets/plugin.Payloads take no tenantID parameter on
// the request-path methods; ForTenant is the one place a plugin still names a
// tenant directly, for a background loop with no request (mirroring
// plugin.ForTenant on the SQL side, cleat#2125) or an unauthenticated request
// that names its own tenant. Secrets.ForTenant's own doc comment
// (plugin/secrets.go) is explicit that this is not the crossTenantLedger
// shape -- every call still names exactly one tenant, not a bypass -- and not
// exactly the perTenantLoopLedger shape either, since a Secrets.ForTenant
// call site is not necessarily inside an AllTenantIDs loop. It is the same
// KIND of question a reviewer asks of the other two ledgers, though: "where
// does this plugin act on a tenant it did not get from its own request", and
// this is that ledger for the Secrets/Payloads surface.
//
// EXPECTED TO START EMPTY, AND THAT IS FINE. At cleat#2163, nothing in the
// tree calls Secrets.ForTenant outside this package's own tests (excluded
// below, same as every sibling ledger's tests). The ledger exists so the
// FIRST real caller -- oauthprovider, datadogexport, whichever plugin reaches
// for it next -- adds one declared line in the same diff a reviewer sees,
// rather than the guard being wired up after the fact once something is
// already undeclared.
//
// RECONCILED WITH cleat#2141, which merged first (09910eb3). #2141 added
// perTenantLoopLedger as crossTenantLedger's sibling for plugin.AllTenantIDs
// call sites, documented as C15 in docs/contributor/plugins/plugin-contract.md.
// This ledger is Secrets.ForTenant's equivalent and is documented as C16,
// right after it -- kept as a separate file rather than folded into
// perTenantLoopLedger's, because the two check different shapes (a
// bypass-spanning AllTenantIDs loop versus a single named tenant) closely
// enough that folding them would make one file's diff answer a question about
// the other's scanner.
var secretsForTenantLedger = map[string]bool{}

// secretsForTenantSite is one Secrets.ForTenant-shaped call, located by
// go/ast -- same reasoning as crossTenantSite and perTenantLoopSite: a regex
// cannot tell a call from a comment describing one, and a parser does not
// need to try.
type secretsForTenantSite struct {
	File string // repo-relative
	Func string // enclosing function, "(*Recv).Name" for a method
	Line int
}

func (s secretsForTenantSite) key() string { return s.File + ":" + s.Func }

// isSecretsForTenant matches X.ForTenant(...) for any X other than the
// literal package identifier "plugin".
//
// That exclusion is the whole trick, and it needs no type information to be
// exact here: plugin.ForTenant(ctx, tenantID) -- the free function that marks
// a context for a plugin's own SQL, cleat#2125 -- is a *ast.SelectorExpr
// whose X is the *ast.Ident "plugin" when called from outside the package,
// and a bare *ast.Ident "ForTenant" (not a SelectorExpr at all) when called
// from within it. Neither shape can collide with Secrets.ForTenant /
// TenantSecrets, which is always called on a value -- a local variable, a
// struct field, never the bare package identifier -- so this matcher accepts
// every SelectorExpr named ForTenant except the one spelling the free
// function uses.
func isSecretsForTenant(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ForTenant" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "plugin" {
		return false
	}
	return true
}

// scanSecretsForTenantSites parses each file and returns every
// Secrets.ForTenant-shaped call. Shares trackedGoFiles/funcName/repoRoot with
// the other two ledger scanners (a_cross_tenant_bypass_is_declared_test.go,
// same package).
func scanSecretsForTenantSites(t *testing.T, root string, files []string) []secretsForTenantSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []secretsForTenantSite

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
				if !ok || !isSecretsForTenant(call.Fun) {
					return true
				}
				sites = append(sites, secretsForTenantSite{
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

// TestEverySecretsForTenantCallIsDeclared is secretsForTenantLedger's
// ratchet, checked in both directions for the same reason
// TestEveryCrossTenantBypassIsDeclared and TestEveryPerTenantLoopIsDeclared
// are: an undeclared site is a new tenant-naming call nobody reviewed, and a
// stale entry is a grant covering something that is not there.
//
// UNLIKE its two siblings, an empty ledger passing is expected and correct
// today (see secretsForTenantLedger's own doc) -- so this test does NOT
// assert len(found) > 0 the way the other two do; that assertion belongs to
// TestTheSecretsForTenantScannerReportsAnUndeclaredSite instead, which
// exercises the matcher against a fixture rather than against the live tree.
func TestEverySecretsForTenantCallIsDeclared(t *testing.T) {
	root := repoRoot(t)
	sites := scanSecretsForTenantSites(t, root, trackedGoFiles(t, root))

	found := map[string]secretsForTenantSite{}
	for _, s := range sites {
		found[s.key()] = s
	}

	for key, site := range found {
		if !secretsForTenantLedger[key] {
			t.Errorf("undeclared Secrets.ForTenant call at %s:%d\n"+
				"  Secrets.ForTenant/TenantSecrets is a plugin naming a tenant it did not get\n"+
				"  from its own request. Add %q to secretsForTenantLedger, or thread the tenant\n"+
				"  through the request-path methods instead if one is actually available on this\n"+
				"  path -- a ForTenant call that did not need to exist is the failure this ledger\n"+
				"  is for.", site.File, site.Line, key)
		}
	}
	for key := range secretsForTenantLedger {
		if _, ok := found[key]; !ok {
			t.Errorf("stale secretsForTenantLedger entry %q: no Secrets.ForTenant call there.\n"+
				"  The call was removed or its function was renamed. A ledger line that matches\n"+
				"  nothing is a grant covering something that is not there -- it will silently\n"+
				"  cover the NEXT thing to take that name.", key)
		}
	}
}

// TestTheSecretsForTenantScannerReportsAnUndeclaredSite is the known-positive.
// TestEverySecretsForTenantCallIsDeclared answers "does it pass when the tree
// is fine?", which every broken version of it also answers yes to -- and,
// unlike its two siblings, "passes on the live tree" is trivially true today
// since the ledger is empty and the scanner has found nothing to check itself
// against. This is the test that answers the harder question: does it REPORT
// a case that is genuinely wrong?
func TestTheSecretsForTenantScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "secretsfortenant", "undeclared.go")

	sites := scanSecretsForTenantSites(t, root, []string{fixture})
	if len(sites) != 1 {
		t.Fatalf("scanner found %d site(s) in the fixture, want 1 -- it can no longer see "+
			"what it is looking for, and would report a real undeclared Secrets.ForTenant call "+
			"as absent", len(sites))
	}
	if secretsForTenantLedger[sites[0].key()] {
		t.Fatalf("the fixture site %s is in the ledger; it must stay undeclared, "+
			"or this test proves nothing", sites[0].key())
	}
}

// TestTheSecretsForTenantScannerIgnoresThePluginForTenantFreeFunction is the
// negative control for isSecretsForTenant's exclusion: plugin.ForTenant(ctx,
// tenantID), the free function marking a context for a plugin's own SQL, must
// NOT be reported here -- it is a different mechanism, already covered by
// neither crossTenantLedger nor perTenantLoopLedger by design (see
// Secrets.ForTenant's own doc comment). Without this control, a matcher that
// accepted EVERY ".ForTenant(" call -- including the free function's own
// dozen-plus real call sites across plugins/ -- would make
// TestEverySecretsForTenantCallIsDeclared fail on the live tree today, and
// the failure would look like this guard working rather than like it being
// wrong.
func TestTheSecretsForTenantScannerIgnoresThePluginForTenantFreeFunction(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "barecontext", "unscoped.go")

	sites := scanSecretsForTenantSites(t, root, []string{fixture})
	if len(sites) != 0 {
		t.Fatalf("scanner found %d Secrets.ForTenant-shaped site(s) in a fixture that only "+
			"calls the free function plugin.ForTenant, want 0 -- it can no longer tell the two "+
			"apart, which would make every real plugin.ForTenant call site an undeclared "+
			"Secrets.ForTenant finding", len(sites))
	}
}
