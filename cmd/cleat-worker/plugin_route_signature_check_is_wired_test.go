package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCheckPluginRouteSignaturesIsCalledFromMain pins the call site cleat#2277
// added, the same way TestEveryPreparedLoopIsLaunched (loop_launch_test.go)
// pins launchLoop: checkPluginRouteSignatures is a pure function with its own
// unit test above, and nothing about main.go calling it is otherwise
// exercised by a behavioural test -- main() wires it into a chain of closures
// nothing outside main() can reach (see a_slack_interactive_route_is_exempt_test.go's
// doc comment for why). Deleting the call -- or a future refactor that stops
// checking its returned error -- would leave every other test in this
// package green, and would silently restore the pre-#2277 behavior: a
// plugin's RegisterRoutes method drifting to the pre-#2232 signature would
// again only be discoverable by reading logs, not by the worker refusing to
// start.
func TestCheckPluginRouteSignaturesIsCalledFromMain(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "checkPluginRouteSignatures" {
			found = true
		}
		return true
	})

	if !found {
		t.Fatal("main.go no longer calls checkPluginRouteSignatures -- a plugin whose " +
			"RegisterRoutes has the pre-cleat#2232 signature would once again only be " +
			"logged about, not refused at boot (cleat#2277)")
	}
}
