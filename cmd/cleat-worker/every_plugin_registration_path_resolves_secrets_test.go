package main

// cleat#1987. Register wraps every plugin function in withSecrets; until this
// issue, RegisterStream did not wrap at all, and ${secret:NAME} reached a
// streaming plugin function -- llm.chat_stream -- as literal reference text.
//
// The behavioral tests in secret_substitution_test.go prove the two paths
// that exist TODAY are wrapped. They cannot prove anything about a path added
// tomorrow: a new registration method with no equivalent wrapper would simply
// not appear in that file, and every existing test would keep passing. This
// is the guard for that -- read from the source with go/ast, the same
// reasoning cmd/cleatctl/every_subcommand_declares_its_dialects_test.go gives
// for reading main.go's dispatch switch instead of keeping a list: a check
// that IS the source cannot go stale against it.
//
// SCOPED TO METHODS NAMED "Register*" ON *hostPluginRegistryAdapter. That is
// the adapter's actual registration surface -- it implements
// plugin.FuncRegistry (Register) and plugin.StreamFuncRegistry
// (RegisterStream), and both interfaces name their method that way. A method
// added to satisfy a third such interface follows the same convention or the
// engine's registry lookup by name would already be inconsistent with it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryPluginRegistrationPathResolvesSecrets(t *testing.T) {
	root := workerRepoRoot(t)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "cleat-worker", "setup.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/cleat-worker/setup.go: %v", err)
	}

	var registrationMethods []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "Register") {
			return true
		}
		if !receiverIsHostPluginRegistryAdapter(fn.Recv) {
			return true
		}
		registrationMethods = append(registrationMethods, fn.Name.Name)

		if !bodyCallsAWithSecretsWrapper(fn.Body) {
			t.Errorf("%s is a registration method on hostPluginRegistryAdapter but its body "+
				"does not call a withSecrets* wrapper before delegating. A registration path "+
				"that skips this lets ${secret:NAME} reach a plugin as literal text -- exactly "+
				"cleat#1987, for RegisterStream. If this method genuinely has nothing to "+
				"resolve (it issues no plugin call at all), say so in this test rather than "+
				"leaving the guard unable to see it.", fn.Name.Name)
		}
		return true
	})

	// A floor, not an equality, for the reason every such floor in this repo
	// is: the population here is fixed by an interface contract (exactly
	// Register and RegisterStream today), but a THIRD interface method is
	// exactly the case this test exists to catch, and a floor of 2 lets it
	// arrive without the test needing to be told its name in advance. What it
	// must not do is silently see zero, which is the "extraction broke"
	// failure this repo's own review notes call out (cleat#1723's marker
	// scan, cleat#986's censuses).
	if len(registrationMethods) < 2 {
		t.Fatalf("found only %d Register* method(s) on hostPluginRegistryAdapter in setup.go "+
			"(%v); there were 2 (Register, RegisterStream) on 2026-09-23. This test reads the "+
			"source with go/ast, so too few means the extraction broke, not that a registration "+
			"path was removed.", len(registrationMethods), registrationMethods)
	}
}

// receiverIsHostPluginRegistryAdapter checks the receiver names the adapter
// type, pointer or value, so a rename of the receiver variable (a to r, say)
// cannot make this stop matching.
func receiverIsHostPluginRegistryAdapter(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) != 1 {
		return false
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "hostPluginRegistryAdapter"
}

// bodyCallsAWithSecretsWrapper reports whether fn's body contains a call
// whose selector name starts with "withSecrets" -- withSecrets or
// withSecretsStream today, and anything named in the same family tomorrow.
// Matched on the PREFIX rather than either exact name so this does not need
// editing when a third wrapper variant is added; it would need editing if a
// wrapper broke the naming convention entirely, which is a much louder
// review signal than a passing test.
func bodyCallsAWithSecretsWrapper(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if strings.HasPrefix(sel.Sel.Name, "withSecrets") {
			found = true
		}
		return true
	})
	return found
}

// workerRepoRoot is defined in no_tenant_pools_test.go, in this same package;
// reused rather than redeclared.
