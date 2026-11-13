// tscheck is the static half of the TypeScript guest's determinism story (plan §4.5, "Static check").
//
// What it is FOR. The runtime shims make most nondeterminism impossible, so this check can be modest. It refuses a
// workflow that names a global the Javy guest does not define at all (timers, WeakRef, performance, crypto, shared memory,
// ambient I/O): at runtime each is a ReferenceError in the middle of a run, which is deterministic but late. Refusing at
// build time gives the author the file and line instead.
//
// What it is NOT. Date.now, new Date() and Math.random are NOT refused: they are shimmed onto the recorded host clock and
// random value, and refusing them would make the shims pointless. A name reached through a computed property
// (globalThis["set" + "Timeout"]), an alias, eval, or a dependency's own bundled code is not seen; the runtime is the
// second layer for those. The full list of limits is in PHASE2.md and pinned by tscheck_test.go.
//
// Three outcomes, one exit status each, because a check that could not look must not read as a check that passed:
//
//	0  clean       (and it says how many identifier tokens it looked at, so "scanned nothing" is visible)
//	1  finding     file:line:col: TSnnn name: reason
//	2  UNMEASURED  the file could not be read or tokenised; this is a failure of the check, not a finding about the file
package main

import (
	"fmt"
	"os"
	"sort"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tscheck file.ts [file.ts ...]")
		os.Exit(2)
	}
	exit := 0
	for _, path := range os.Args[1:] {
		res := checkFile(path)
		switch {
		case res.unmeasured != "":
			fmt.Printf("%s: UNMEASURED: %s\n  this is a failure of the check, not a finding about the file\n", path, res.unmeasured)
			if exit < 2 {
				exit = 2
			}
		case len(res.findings) > 0:
			sort.Slice(res.findings, func(i, j int) bool { return res.findings[i].line < res.findings[j].line })
			for _, f := range res.findings {
				fmt.Printf("%s:%d:%d: %s %s: %s\n", path, f.line, f.col, f.code, f.name, f.reason)
			}
			if exit == 0 {
				exit = 1
			}
		default:
			fmt.Printf("%s: clean (scanned %d identifier tokens", path, res.tokens)
			if len(res.allowed) > 0 {
				fmt.Printf("; ALLOWED by marker: %v", res.allowed)
			}
			fmt.Println(")")
		}
	}
	os.Exit(exit)
}
