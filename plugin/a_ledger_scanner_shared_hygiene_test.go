package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// Shared hygiene helpers for every total-coverage ledger scanner in this
// package (crossTenantLedger, perTenantLoopLedger, secretsForTenantLedger,
// pluginForTenantLedger). cleat#2740.
//
// THE FAMILY PROPERTY THESE CLOSE. Every one of those scanners inspects
// `CallExpr.Fun` for a specific shape -- a SelectorExpr whose package
// identifier is the literal "plugin", or a bare Ident for the unqualified
// spelling -- and nothing else. That is one blind spot in two directions:
//
//   (F) A renamed import: `import pl "…/plugin"; pl.ForTenant(ctx, id)` is a
//       SelectorExpr whose X is "pl", not "plugin". It fails EVERY scanner's
//       package-identifier check, including the one it should have matched,
//       and (per isSecretsForTenant's exclusion, which accepts every
//       ForTenant-named selector EXCEPT the one spelled "plugin.ForTenant")
//       gets wrongly claimed by secretsForTenantLedger's scanner instead.
//       Loud, and misfiled -- the worst combination, because nothing prompts
//       anyone to notice the RIGHT guard never fired.
//   (G) An indirect reference: `mark := plugin.ForTenant; mark(ctx, id)`
//       assigns the tracked selector to a variable. The ledger only ever
//       looks at `CallExpr.Fun`, and here that is the bare Ident `mark`, not
//       the SelectorExpr this scanner is looking for. Silent in every
//       scanner. A new call site can be added to a tracked function without
//       the ledger noticing, which is exactly what these guards exist to
//       prevent.
//
// (F) is fixed by refusing to scan a file whose "…/plugin" import uses any
// name other than the default. (G) is fixed by flagging any occurrence of a
// tracked shape that is not itself the Fun of some CallExpr in the same
// function -- an escape, in the sense that the ledger's tracking of it
// escapes through the variable.

// pluginImportName returns the alias this file's "github.com/cleat-team/
// cleat/plugin" import is used under, and true if the file does not import
// that package at all -- a scanner has nothing to refuse in that case, since
// none of its matchers can fire. If the import exists under a name other
// than the default "plugin" (an explicit alias, "_", or "."), ok is false:
// every ledger scanner hardcodes the literal "plugin" as the package
// identifier it recognises, so a file that imports it otherwise is a shape
// none of them can read correctly, and the caller should refuse to scan it
// rather than silently mis-scan it.
func pluginImportName(f *ast.File) (name string, ok bool) {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "github.com/cleat-team/cleat/plugin" {
			continue
		}
		if imp.Name == nil {
			return "plugin", true
		}
		return imp.Name.Name, imp.Name.Name == "plugin"
	}
	return "", true
}

// assertDefaultPluginImportName is (F)'s guard: called once per parsed file,
// before any of the four ledger scanners inspect it. Fails loudly, naming
// the file and the alias found, rather than letting every scanner silently
// fail to recognise (or, per isSecretsForTenant's exclusion, wrongly claim)
// a call made through a renamed import.
func assertDefaultPluginImportName(t *testing.T, f *ast.File, rel string) {
	t.Helper()
	name, ok := pluginImportName(f)
	if ok {
		return
	}
	t.Fatalf("%s imports github.com/cleat-team/cleat/plugin as %q, not the "+
		"default \"plugin\"\n"+
		"  Every total-coverage ledger scanner in this package (cross-tenant,\n"+
		"  per-tenant-loop, secrets-for-tenant, plugin-for-tenant) hardcodes the\n"+
		"  literal package identifier \"plugin\" and cannot recognise a call made\n"+
		"  through any other name -- a renamed import is either invisible to every\n"+
		"  scanner, or (for the ForTenant shape specifically) misattributed to the\n"+
		"  wrong one. Use the default import name in this file, or extend the\n"+
		"  scanner family to know about the alias before adding one.", rel, name)
}

// trackedReferenceEscapes reports the source line of every occurrence of a
// tracked selector/identifier inside fn's BODY that is not itself the Fun of
// some CallExpr in the same function -- (G)'s guard. isTracked is the same
// matcher function the scanner already uses (isAcrossAllTenants,
// isAllTenantIDs, isSecretsForTenant, isPluginForTenantCall).
//
// fn.Body, NOT fn. A FuncDecl's own Name is an *ast.Ident too, and for a
// scanner whose Ident-branch matches a bare name like "ForTenant" or
// "AllTenantIDs", that name collides with the DEFINITION of the tracked
// function itself: plugin.ForTenant's own declaration is `func
// ForTenant(...)`, and a method on an unrelated type that happens to be
// named ForTenant (every Secrets/Payloads implementation has one, since it
// is the interface method, not a call to the free function) declares the
// identical Ident. Walking the whole FuncDecl reported both as "referenced
// as a value" on first run -- seven false positives across
// engine/plugin_secrets.go, plugin/crosstenant.go (the free function's own
// definition), plugins/datadogexport, plugins/notifications,
// plugins/plugintest, plugins/webhookingest and tests/plugin-harness, all
// declarations, none of them a reference at all. A declaration's name is
// never a value use; only the body can contain one.
//
// WHY A SelectorExpr's .Sel FIELD IS NEVER VISITED ON ITS OWN. go/ast's
// Walk descends into a SelectorExpr's X and Sel fields independently, and
// .Sel is always a bare *ast.Ident -- the field/method NAME, e.g. the
// "ForTenant" in `p.secrets.ForTenant`. That Ident is not a reference to
// anything by itself; it only means something as part of its parent
// SelectorExpr. A first version of this function let ast.Inspect's default
// descent walk into .Sel whenever the SelectorExpr AS A WHOLE did not match
// isTracked (e.g. `p.secrets.ForTenant`, whose X is `p.secrets`, not the bare
// Ident "plugin" isPluginForTenantCall requires) -- and the lone Ident
// "ForTenant" it then visited independently DID match the Ident-branch every
// isTracked function has, for the unqualified-call-from-within-the-package
// case. That produced four false positives on the real tree
// (plugins/datadogexport, plugins/notifications, plugins/webhookingest,
// tests/plugin-harness), all `x.y.ForTenant(...)` chains where y is not the
// literal identifier "plugin". Fixed by never letting ast.Inspect
// auto-descend past a SelectorExpr: this function recurses into .X itself,
// explicitly, and never into .Sel.
func trackedReferenceEscapes(fn *ast.FuncDecl, fset *token.FileSet, isTracked func(ast.Expr) bool) []int {
	if fn.Body == nil {
		return nil
	}
	callFuns := map[ast.Expr]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			callFuns[call.Fun] = true
		}
		return true
	})

	var lines []int
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		sel, isSel := n.(*ast.SelectorExpr)
		if isSel {
			if isTracked(sel) && !callFuns[sel] {
				lines = append(lines, fset.Position(sel.Pos()).Line)
			}
			ast.Inspect(sel.X, visit)
			return false // never descend into .Sel independently
		}
		if id, isID := n.(*ast.Ident); isID {
			if isTracked(id) && !callFuns[id] {
				lines = append(lines, fset.Position(id.Pos()).Line)
			}
			return false // a leaf; nothing to descend into anyway
		}
		return true
	}
	ast.Inspect(fn.Body, visit)
	return lines
}

// assertNoTrackedReferenceEscapes is (G)'s guard, called from the same
// per-function loop that already calls the scanner's own CallExpr check.
func assertNoTrackedReferenceEscapes(t *testing.T, fn *ast.FuncDecl, fset *token.FileSet, rel, funcName string, isTracked func(ast.Expr) bool) {
	t.Helper()
	for _, line := range trackedReferenceEscapes(fn, fset, isTracked) {
		t.Errorf("%s:%d: in %s, a tracked function is referenced as a value, not called directly\n"+
			"  A ledger keyed on CallExpr.Fun cannot follow a reference that escapes into a\n"+
			"  variable, a struct field, or an argument -- whatever call it eventually\n"+
			"  drives, if any, no longer carries the name this scanner looks for. Call it\n"+
			"  directly instead, or -- if the indirection is genuinely needed -- this\n"+
			"  scanner family needs extending before this line is safe to add.",
			rel, line, funcName)
	}
}

// TestPluginImportNameRefusesARenamedImport is (F)'s known-positive. Calls
// pluginImportName directly rather than the t.Fatalf wrapper, so a failure
// here is a normal assertion rather than aborting the test.
func TestPluginImportNameRefusesARenamedImport(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "plugin", "testdata", "importalias", "renamed_import.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	name, ok := pluginImportName(f)
	if ok {
		t.Fatalf("pluginImportName said ok=true for a file importing cleat/plugin as %q -- "+
			"it can no longer see a renamed import, and would let every scanner silently "+
			"mis-read one", name)
	}
	if name != "pl" {
		t.Fatalf("pluginImportName returned alias %q, want %q", name, "pl")
	}
}

// TestPluginImportNameAcceptsTheDefaultAlias is the negative control: a file
// importing cleat/plugin under its default name -- the overwhelming majority
// of the tree -- must not be refused. Reuses the existing barecontext
// fixture, which already imports plugin normally.
func TestPluginImportNameAcceptsTheDefaultAlias(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "plugin", "testdata", "barecontext", "unscoped.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if _, ok := pluginImportName(f); !ok {
		t.Fatal("pluginImportName refused a file importing cleat/plugin under its default name -- " +
			"it would refuse to scan the overwhelming majority of the tracked tree")
	}
}

// parseFuncDecl parses rel and returns the *ast.FuncDecl named fn, for tests
// that need to hand a single function to trackedReferenceEscapes directly.
func parseFuncDecl(t *testing.T, root, rel, fn string) (*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == fn {
			return d, fset
		}
	}
	t.Fatalf("%s has no func %s", rel, fn)
	return nil, nil
}

// TestTrackedReferenceEscapesCatchesAnIndirectCall is (G)'s known-positive,
// the issue's own example: `mark := plugin.ForTenant; mark(ctx, id)`.
func TestTrackedReferenceEscapesCatchesAnIndirectCall(t *testing.T) {
	root := repoRoot(t)
	rel := filepath.Join("plugin", "testdata", "escapedref", "value_reference.go")
	fn, fset := parseFuncDecl(t, root, rel, "indirectlyThroughAVariable")

	lines := trackedReferenceEscapes(fn, fset, isPluginForTenantCall)
	if len(lines) != 1 {
		t.Fatalf("trackedReferenceEscapes found %d escape(s), want 1 -- it can no longer see "+
			"`mark := plugin.ForTenant`, and would let a real indirect call through undetected", len(lines))
	}
}

// TestTrackedReferenceEscapesIgnoresADirectCall is the negative control: a
// plain, direct plugin.ForTenant(...) call -- theCorrectForm, already used
// as the OTHER scanners' known-positive fixture -- must not be reported.
// Without this, a version of trackedReferenceEscapes that flagged every
// tracked selector regardless of context would pass its own known-positive
// while making TestEveryPluginForTenantCallIsDeclared fail on every real
// call site in the tree.
func TestTrackedReferenceEscapesIgnoresADirectCall(t *testing.T) {
	root := repoRoot(t)
	rel := filepath.Join("plugin", "testdata", "barecontext", "unscoped.go")
	fn, fset := parseFuncDecl(t, root, rel, "theCorrectForm")

	lines := trackedReferenceEscapes(fn, fset, isPluginForTenantCall)
	if len(lines) != 0 {
		t.Fatalf("trackedReferenceEscapes reported %v for a direct call -- "+
			"it would fail TestEveryPluginForTenantCallIsDeclared on every real call site in the tree", lines)
	}
}

// TestThePluginForTenantScannerCatchesTheBareUnqualifiedForm closes (E):
// isPluginForTenantCall's `case *ast.Ident` arm, matching a call to the
// unqualified name (the shape a call from WITHIN package plugin itself
// would use), had no fixture and no live caller -- deleting that arm failed
// nothing. This fixture gives it one.
func TestThePluginForTenantScannerCatchesTheBareUnqualifiedForm(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "barecontext", "bare_unqualified_call.go")

	sites := scanPluginForTenantSites(t, root, []string{fixture})
	if len(sites) != 1 {
		t.Fatalf("scanner found %d site(s) in the bare-unqualified-call fixture, want 1 -- "+
			"the `case *ast.Ident` arm can no longer see this shape", len(sites))
	}
}
