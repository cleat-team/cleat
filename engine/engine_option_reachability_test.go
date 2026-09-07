package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// engineOptionsNotWired are EngineOption constructors deliberately not called
// by any production code, with the reason.
//
// EMPTY, and it may only grow with a reason that survives review. An option
// nothing calls is a configuration knob the product cannot turn: the code
// reading the field is complete, the code setting it does not exist, and every
// test that constructs an Engine directly passes the option itself and so
// cannot see the gap.
//
// That is exactly what happened to WithPluginStreamRegistry. It had seven
// references, all of them in engine/unit_test.go, and cmd/cleat-worker built a
// PluginStreamRegistry, registered every plugin's streaming functions into it,
// carried a field for it on the Worker struct -- and never assigned that field
// or passed the option. Streaming plugin calls answered "no plugin stream
// registry configured" in every real worker for as long as they had existed.
var engineOptionsNotWired = map[string]string{
	// VERIFIED: a dead alternate path, not a break. cmd/cleat-worker does its
	// own continue-as-new at setup.go:1841, calling execStore.ContinueAsNew
	// directly, so the engine's branches at executor.go:337 and :492 -- both
	// guarded on this handler being non-nil -- never fire in a real worker.
	// Continue-as-new works; the engine carries a second mechanism for it that
	// nothing selects. See cleat#826, which is about the same feature.
	"WithContinueAsNewHandler": "cleat#878: the worker calls execStore.ContinueAsNew directly, so the engine's handler branch is a dead alternate path",

	// NOT TRIAGED. Each of these is either a legitimate test seam -- injecting
	// a fake clock is a real reason for an option only tests set -- or an
	// unwired knob like WithPluginStreamRegistry was. Telling those apart takes
	// reading each one, and guessing in bulk is how a baseline becomes a
	// standing exemption. Recorded honestly rather than described as though it
	// had been checked; cleat#878 triages them.
	"WithAllowVersionMismatch":        "cleat#878: not triaged -- test seam or unwired knob",
	"WithAmbiguityResolver":           "cleat#878: not triaged -- test seam or unwired knob",
	"WithBackend":                     "cleat#878: not triaged -- test seam or unwired knob",
	"WithClock":                       "cleat#878: not triaged -- test seam or unwired knob",
	"WithDeferPassBudget":             "cleat#878: not triaged -- test seam or unwired knob",
	"WithFetcher":                     "cleat#878: not triaged -- test seam or unwired knob",
	"WithPluginCallGuard":             "cleat#878: not triaged -- test seam or unwired knob",
	"WithPluginCallObserver":          "cleat#878: not triaged -- test seam or unwired knob",
	"WithWasmCumulativeAllocationMax": "cleat#878: not triaged -- test seam or unwired knob",
}

// TestEveryEngineOptionIsReachableFromProduction asserts that each With* option
// the engine exports is actually called outside tests.
//
// The direction matters: the usual dead-code question is "does anything mention
// this name", which a test mentioning it answers yes. This asks the narrower
// question that matters for a knob -- does anything that SHIPS set it.
func TestEveryEngineOptionIsReachableFromProduction(t *testing.T) {
	root := moduleRoot(t)

	declared := map[string]string{} // option name -> file it is declared in
	callers := map[string][]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		isTest := strings.HasSuffix(path, "_test.go")

		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil // not this guard's business
		}
		rel, _ := filepath.Rel(root, path)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "With") {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); ok && id.Name == "EngineOption" && !isTest {
				declared[fn.Name.Name] = rel
			}
		}

		// A call is a call whether written engine.WithX(...) or WithX(...) from
		// inside the package. An *ast.CallExpr is neither of the two things a
		// text search confuses with one: a comment, or a string literal naming
		// the option in a message about it.
		if isTest {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if strings.HasPrefix(name, "With") {
				callers[name] = append(callers[name], rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("found no EngineOption constructors at all, so this guard checked nothing")
	}

	for name, declaredIn := range declared {
		why, baselined := engineOptionsNotWired[name]
		wired := len(callers[name]) > 0

		switch {
		case wired && baselined:
			t.Errorf("%s IS called by production code now (%s), but is still listed in "+
				"engineOptionsNotWired.\n\nDelete the entry -- a baseline that no longer "+
				"describes anything is a standing exemption for whatever is added next.\n"+
				"Reason on file: %s", name, callers[name][0], why)
		case !wired && !baselined:
			t.Errorf("%s (declared in %s) is never called outside tests, so nothing that "+
				"ships can set it.\n\nEvery test that builds an Engine passes the option "+
				"itself, which is why the suite cannot see this. Wire it where the value is "+
				"built, or add it to engineOptionsNotWired with a reason.", name, declaredIn)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory, so the tree cannot be walked")
		}
		dir = parent
	}
}
