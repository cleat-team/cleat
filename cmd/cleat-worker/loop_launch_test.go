package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// Every background loop that gets a context must also be started.
//
// # The defect this exists for
//
// `compactionLoop` was defined, tested, given a `--compaction-threshold` flag
// and a `--compaction-interval` flag, plumbed through main.go, given a health
// tracker interval, and given `initLoopCtx("compaction")` -- and never
// launched. History compaction had NEVER run in any deployment (cleat#877).
//
// Everything around it was present, which is what made it invisible. The
// missing piece was one line, and the piece most likely to be missing is always
// the one that STARTS something: every other piece of a loop is referenced by
// the thing above it, so an unreferenced loop body looks exactly like a
// referenced one from anywhere except the launch site.
//
// # Why initLoopCtx is the right anchor
//
// It is the one call that says "this loop is meant to exist" without also
// starting it. A loop with a context and no launch is a loop somebody intended
// and did not wire; a loop with neither is not a loop. Anchoring on the
// function definition instead would report every helper named `*Loop`, and
// anchoring on the flag would report configuration that legitimately has no
// loop.
//
// The stale direction matters as much: `initLoopCtx("update_dispatch")`
// outlived the loop it prepared when the 5s update ticker was deleted
// (IMPROVEMENT-PLAN 3.239), and nothing noticed. This test reports that too.
func TestEveryPreparedLoopIsLaunched(t *testing.T) {
	prepared, launched, registered := loopNamesIn(t, "setup.go")

	if len(prepared) < 5 {
		t.Fatalf("found only %d initLoopCtx calls; there were 9 on 2026-09-07. "+
			"A scan that finds almost nothing passes vacuously.", len(prepared))
	}

	var neverLaunched, neverPrepared []string
	for name := range prepared {
		if !launched[name] {
			neverLaunched = append(neverLaunched, name)
		}
	}
	for name := range launched {
		if !prepared[name] {
			neverPrepared = append(neverPrepared, name)
		}
	}
	sort.Strings(neverLaunched)
	sort.Strings(neverPrepared)

	for _, name := range neverLaunched {
		t.Errorf("loop %q has initLoopCtx but no launchLoop: it is prepared and never started.\n\n"+
			"Either add the launchLoop call, or drop the initLoopCtx if the loop is gone. "+
			"This is the defect cleat#877 was -- a complete subsystem behind a missing "+
			"start, where every other piece being present is what stopped anyone looking.",
			name)
	}
	for _, name := range neverPrepared {
		t.Errorf("loop %q is launched but has no initLoopCtx, so getLoopCtx(%q) has no entry "+
			"and the loop cannot be cancelled or restarted by the watchdog.", name, name)
	}

	// A launched loop should also be registered, or the watchdog cannot restart
	// it after a panic. Reported separately: it is a weaker failure than never
	// starting, and conflating them would let a fix for one look like a fix for
	// both.
	for name := range launched {
		if !registered[name] {
			t.Errorf("loop %q is launched but never passed to registerLoopFunc, so the "+
				"watchdog has no function to restart it with.", name)
		}
	}

	t.Logf("%d loops prepared, %d launched, %d registered", len(prepared), len(launched), len(registered))
}

// loopNamesIn collects the string-literal argument of each initLoopCtx,
// launchLoop and registerLoopFunc call.
//
// From the AST, so a loop name inside a comment or a log message is not
// mistaken for a call -- the trap this repo keeps paying for. An *ast.CallExpr
// is neither.
func loopNamesIn(t *testing.T, file string) (prepared, launched, registered map[string]bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	prepared, launched, registered = map[string]bool{}, map[string]bool{}, map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var fn string
		switch v := call.Fun.(type) {
		case *ast.Ident:
			fn = v.Name // initLoopCtx is a local closure in Run
		case *ast.SelectorExpr:
			fn = v.Sel.Name
		default:
			return true
		}

		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// A non-literal name cannot be checked statically. Not silently
			// ignored: see the vacuity floor on the caller, which is what
			// notices if this branch starts swallowing everything.
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}

		switch fn {
		case "initLoopCtx":
			prepared[name] = true
		case "launchLoop":
			launched[name] = true
		case "registerLoopFunc":
			registered[name] = true
		}
		return true
	})
	return prepared, launched, registered
}
