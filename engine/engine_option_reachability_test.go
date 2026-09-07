package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
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

	// TRIAGED 2026-09-07 (cleat#878). Four categories emerged, and the third
	// was not one the issue anticipated:
	//
	//   TEST SEAM          an option only tests set, for a real reason.
	//   EMBEDDER API       `cleat/` is a public Go API. An option that exists
	//                      for an external embedder has no in-repo caller by
	//                      design, and "nothing that ships sets it" is the
	//                      expected answer rather than a finding.
	//   ALTERNATE PATH     a second implementation of something the worker
	//                      already does its own way. Not broken; unselected.
	//   REAL GAP           the feature does not work in production.
	//
	// One real gap was found: WithFetcher. See IMPROVEMENT-PLAN.md 3.317.

	// REAL GAP. ABI.md 2.48 documents cleat_fetch as "Perform an HTTP fetch
	// request", with no caveat. Nothing anywhere sets a fetcher -- the only
	// reference to WithFetcher outside tests is its own declaration -- so
	// engine/lifecycle.go takes the else branch and every fetch from a real
	// worker returns "no fetcher configured: workflow %s attempted %s %s".
	// This is #879's shape exactly: a complete, documented read path behind a
	// write path nobody wired. Kept baselined because wiring it is a product
	// decision (a default fetcher gives every workflow arbitrary outbound
	// HTTP), not because it is acceptable. 3.317 carries the decision.
	"WithFetcher": "cleat#878 REAL GAP: cleat_fetch always fails in a real worker -- see 3.317",

	// ALTERNATE PATH. The worker enforces this limit itself:
	// --wasm-cumulative-allocation-max-mb feeds w.wasmCumulativeAllocationMaxBytes
	// and tryClaimCumulativeAllocation (cmd/cleat-worker/setup.go), never this
	// option. Zero references anywhere in the tree, tests included -- the only
	// one of the ten with none. Same shape as WithContinueAsNewHandler.
	"WithWasmCumulativeAllocationMax": "cleat#878 ALTERNATE PATH: worker does its own via tryClaimCumulativeAllocation",

	// ALTERNATE PATH. The worker passes WithWasmtimeDeferBudget (a
	// WasmtimeOption, cmd/cleat-worker/main.go) for the same quantity -- the
	// wall-clock ceiling on the cleanup pass. Two knobs for one bound, one
	// selected.
	"WithDeferPassBudget": "cleat#878 ALTERNATE PATH: worker passes WithWasmtimeDeferBudget instead",

	// ALTERNATE PATH. Production uses the PLURAL WithBackends, at
	// cmd/cleat-worker/setup.go. The singular is a 42-site test convenience,
	// which is why it is kept rather than deleted.
	"WithBackend": "cleat#878 TEST SEAM: production uses WithBackends (plural); 42 test call sites",

	// TEST SEAM. A fake clock is the textbook reason for an option only tests
	// set, and four independent test files inject one -- not the single-file
	// signature that WithPluginStreamRegistry had.
	"WithClock": "cleat#878 TEST SEAM: fake clock, injected by four independent test files",

	// CONSEQUENCE, not an independent finding. This guards the call_plugin
	// capability for WASM plugins, and WASM plugin execution is itself unwired:
	// PluginLoader.LoadPlugin has no non-test callers (IMPROVEMENT-PLAN 3.315).
	// Wiring the guard before the thing it guards would be backwards.
	"WithPluginCallGuard": "cleat#878: guards WASM plugin calls, which are themselves unwired -- see 3.315",

	// EMBEDDER API, and deliberately degrading. Consulted when replay finds a
	// call dispatched but never recorded; nil returns ("", false), which
	// engine/callintent.go documents as leaving the ambiguity "exactly as it
	// was ... and not a worse one". So an unset resolver is the designed
	// default and the enhancement simply does not happen.
	"WithAmbiguityResolver": "cleat#878 EMBEDDER API: nil is the designed default; degrades to no-op",

	// EMBEDDER API. An escape hatch for a caller that knowingly wants replay
	// against a mismatched version. There is deliberately no CLI flag -- a
	// worker should not offer this -- so no in-repo caller is correct.
	"WithAllowVersionMismatch": "cleat#878 EMBEDDER API: escape hatch, deliberately not a worker flag",

	// EMBEDDER API. A post-invocation observer hook for an embedder that wants
	// to instrument plugin calls. The worker uses metrics and tracing instead.
	"WithPluginCallObserver": "cleat#878 EMBEDDER API: observer hook for embedders; worker uses metrics",
}

// TestEveryEngineOptionIsReachableFromProduction asserts that each With* option
// the engine exports is actually called outside tests.
//
// The direction matters: the usual dead-code question is "does anything mention
// this name", which a test mentioning it answers yes. This asks the narrower
// question that matters for a knob -- does anything that SHIPS set it.
// walkTrackedGoFiles visits each tracked .go file by absolute path.
//
// It keeps the same skip list the old filepath.Walk had, for the directories
// that are tracked but not production Go: testdata is fixtures, docs is prose,
// vendor is other people's code.
func walkTrackedGoFiles(root string, tracked []string, visit func(path string) error) error {
	for _, rel := range tracked {
		switch {
		case strings.HasPrefix(rel, "testdata/"), strings.Contains(rel, "/testdata/"),
			strings.HasPrefix(rel, "docs/"), strings.HasPrefix(rel, "vendor/"),
			strings.Contains(rel, "/vendor/"), strings.Contains(rel, "/node_modules/"):
			continue
		}
		if err := visit(filepath.Join(root, rel)); err != nil {
			return err
		}
	}
	return nil
}

func TestEveryEngineOptionIsReachableFromProduction(t *testing.T) {
	root := moduleRoot(t)

	declared := map[string]string{} // option name -> file it is declared in
	callers := map[string][]string{}

	// `git ls-files`, not filepath.Walk.
	//
	// A walk descends into .claude/worktrees/, which holds whole additional
	// checkouts of this repo. This guard reported WithUpdateHandler as
	// unreachable long after it was deleted (#872) because a STALE WORKTREE
	// still declared it -- and it reported the path, which is how it was found:
	//
	//	WithUpdateHandler (declared in
	//	.claude/worktrees/agent-java-object-results/engine/engine.go)
	//
	// The failure is local-only: CI checks out a clean tree with no worktrees,
	// so the guard passes there and fails on a developer's machine, which is
	// the direction that wastes the most time. CLAUDE.md states the rule --
	// "prefer `git ls-files` over `rglob`/`find` for anything that reasons
	// about 'the repo'" -- and records the same defect in a different guard.
	//
	// ls-files also excludes untracked scratch files for free, which a walk
	// would have to enumerate exclusions for, and would therefore go on
	// missing as new ones appeared.
	out, gerr := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if gerr != nil {
		t.Fatalf("git ls-files: %v", gerr)
	}
	tracked := strings.Fields(string(out))
	if len(tracked) < 200 {
		t.Fatalf("git ls-files returned %d .go files; expected hundreds. A near-empty "+
			"file list makes every option look unreachable, which reads as a wall of "+
			"findings rather than as a broken scan.", len(tracked))
	}

	err := walkTrackedGoFiles(root, tracked, func(path string) error {
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
