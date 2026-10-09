//go:build ignore

// Repro for cleat#3247: gosec's G701 taint analyzer (invoked by golangci-lint's
// gosec linter) reports a sink as tainted or not tainted non-deterministically
// across otherwise-identical runs, on byte-identical source.
//
// Root cause, confirmed here without needing gosec as a dependency (gosec's
// own taint engine is golang.org/x/tools-only internally -- see
// github.com/securego/gosec/v2/taint, version v2.29.0, the version
// golangci-lint v2.14.0 currently embeds):
//
//   - gosec's isParameterTainted (taint/taint.go) walks a function's incoming
//     call-graph edges (node.In) to see whether any caller passes tainted
//     data, but caps the walk at maxCallerEdges = 32 to bound CHA's
//     interface-call over-approximation.
//   - node.In is built by golang.org/x/tools/go/callgraph/cha.CallGraph, which
//     iterates `ssautil.AllFunctions(prog)` -- a Go map -- to decide visitation
//     order, appending edges to each callee's .In slice as it goes
//     (golang.org/x/tools/go/callgraph.AddEdge: `callee.In = append(callee.In, e)`).
//   - Go's map iteration order is randomized per process. So on a function with
//     more than 32 callers, WHICH 32 callers gosec actually examines changes
//     from run to run, even though the full caller SET and the source tree are
//     both identical.
//
// This script demonstrates the mechanism directly: build SSA for engine/
// (including _test.go files, matching what `golangci-lint run ./engine/...`
// analyzes) ONCE, then call cha.CallGraph on that same, unchanged *ssa.Program
// repeatedly within a single process. Holding everything else fixed isolates
// the one variable that matters -- CHA's own map-iteration-driven edge order --
// from every other candidate explanation (process startup, filesystem cache,
// goroutine scheduling under GOMAXPROCS) that CLAUDE.md's "Is this result
// real?" discipline requires ruling out before trusting a mechanism.
//
// Run: go run scripts/gosec-taint-cha-nondeterminism-repro.go
//
// Upstream: this is exactly securego/gosec#1711 / #1712, fixed on gosec's
// master via PR #1733 (merged 2026-09-04: sorts callers and raises the cap to
// 1024) -- but NOT YET in any tagged release as of 2026-10-09. v2.29.0
// (2026-08-26, what golangci-lint v2.14.0 embeds) predates the fix by nine
// days; gosec's master is 31 commits ahead of v2.29.0 with no new tag cut.
// There is nothing to upgrade to yet. See cleat#3247.
package main

import (
	"fmt"
	"os"

	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// maxCallerEdges mirrors gosec's own cap (taint/taint.go:30, v2.29.0) so this
// script's "first N callers" matches exactly what gosec would examine.
const maxCallerEdges = 32

func main() {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo,
		Tests: true, // golangci-lint analyzes _test.go files too; the earlier
		// .golangci.yml comment (cleat#3239) documents gosec's taint rules
		// running package-wide, before any per-file _test.go exclusion.
	}
	pkgs, err := packages.Load(cfg, "./engine/...")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load error:", err)
		os.Exit(1)
	}
	if packages.PrintErrors(pkgs) > 0 {
		fmt.Fprintln(os.Stderr, "package(s) had errors; repro needs a clean tree")
		os.Exit(1)
	}

	prog, _ := ssautil.AllPackages(pkgs, 0)
	prog.Build() // build SSA exactly once; everything below reuses this *ssa.Program unchanged.

	const runs = 20
	var nodeCounts []int
	firstSets := make([]map[string]bool, runs)
	var target *ssa.Function

	for i := 0; i < runs; i++ {
		cg := cha.CallGraph(prog) // fresh map iteration over ssautil.AllFunctions each call

		// Pick, on the first run, whichever function in this program has the
		// most incoming edges -- i.e. the function most exposed to gosec's
		// 32-edge cap. Reuse the same *ssa.Function identity on every later
		// run (it is the same *ssa.Program throughout) so we are comparing
		// the SAME node's edge order across runs, not different nodes.
		if i == 0 {
			maxSeen := -1
			for fn, node := range cg.Nodes {
				if fn == nil {
					continue
				}
				if len(node.In) > maxSeen {
					maxSeen = len(node.In)
					target = fn
				}
			}
			if target == nil {
				fmt.Fprintln(os.Stderr, "no call-graph nodes found")
				os.Exit(1)
			}
		}

		node := cg.Nodes[target]
		if node == nil {
			fmt.Fprintln(os.Stderr, "target function missing from a later run's graph")
			os.Exit(1)
		}
		nodeCounts = append(nodeCounts, len(node.In))

		set := make(map[string]bool)
		for j, e := range node.In {
			if j >= maxCallerEdges {
				break
			}
			if e.Caller != nil && e.Caller.Func != nil {
				set[e.Caller.Func.String()] = true
			}
		}
		firstSets[i] = set
	}

	fmt.Printf("target function (most incoming CHA edges found on run 0): %s\n", target.String())
	fmt.Printf("total incoming edges across %d runs: %v (expect identical -- same program, same full edge SET)\n", runs, nodeCounts)

	distinct := 0
	for i := 1; i < runs; i++ {
		if !sameSet(firstSets[0], firstSets[i]) {
			distinct++
		}
	}
	fmt.Printf("runs whose first-%d-examined caller set differs from run 0's: %d / %d\n", maxCallerEdges, distinct, runs-1)
	fmt.Println("(a non-zero count here, alongside an unchanging total-edges row above, is exactly")
	fmt.Println(" gosec's isParameterTainted hazard: the full caller set never changes, but")
	fmt.Println(" which 32 of it get examined does.)")
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
