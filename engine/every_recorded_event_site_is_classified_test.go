package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cleat#4160. A failed write of a recorded event stops the session at its NEXT fresh host call (stopBeforeNewWork). That is only as good as
// the claim that every host call which does new work consults it, and the recordEvent sites are where a new host call shows up. So every
// function that records an event is either one that calls stopBeforeNewWork itself, or is listed here with the reason it does not need to.
//
// A function that records and is in neither set fails the test: a new recorded event is a decision about what happens after its write fails,
// and it has to be made here. An exception that no longer applies (the function now checks, or no longer records) fails too, so the list
// cannot outlive its reasons.
//
// LIMIT, measured: this reads the source, so a check disabled but still present (`if false && s.stopBeforeNewWork()`) is invisible to it; deleting
// the call is what it sees.
//
// The reasons are of three kinds. OBSERVATION: the event records what a read returned, nothing was done, and the next fresh call is stopped.
// TERMINAL: the event ends the run's segment. FRESH HALF: the fresh half of a host call whose entry point checks (DurableCall, SideEffect...);
// the replay-to-fresh hand-off that reaches these without the check is cleat#4143 and cleat#4157, not this guard's subject. (cleat#4150 moved the
// check INSIDE freshCall, freshCallWithHeartbeat, freshSideEffect (since moved onto step, cleat#4152) and freshPluginCallStreaming, and this list failed until their entries were removed.)
var recordedEventSitesWithoutOwnStopCheck = map[string]string{
	"step_dispatcher.go:Record":          "DISPATCHER: reached only from a spec's Fresh, which step runs after it has applied stopBeforeNewWork to every effectEffect step; the one spec that is never refused, ReleaseLock (`Refuse: neverRefuse`), says why where neverRefuse is declared",
	"plugins.go:freshPluginCallInternal": "FRESH HALF: the plugin-call entry points check before it",
	"plugins.go:recordStreamError":       "TERMINAL: records the failure that ends a streaming call",
}

func TestEveryRecordedEventSiteIsClassified(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	withoutCheck := map[string]bool{}
	recording, withCheck := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			records, stops := 0, 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if se, ok := c.Fun.(*ast.SelectorExpr); ok {
						switch se.Sel.Name {
						case "recordEvent":
							records++
						case "stopBeforeNewWork":
							stops++
						}
					}
				}
				return true
			})
			if records == 0 {
				continue
			}
			recording++
			if stops == 0 {
				withoutCheck[f+":"+fd.Name.Name] = true
			} else {
				withCheck++
			}
		}
	}
	// A scan that finds almost nothing is not a pass. This was a floor of 30 ("measured 2026-11-19: 34 recording functions, 12 without their own
	// check"), and a floor on this population is wrong by construction: every call that moves onto the step dispatcher (cleat#4152) stops being a
	// function that calls recordEvent, because its spec's Fresh records through stepCtx.Record, so the count falls with each migration and the floor
	// failed the first PR that crossed it (G1b, at 29). What does not drift is that the scan sees BOTH kinds of recording function: one that checks
	// the stop and one that does not. A scan reading the wrong thing finds neither or only one. There WAS a small number here as a sanity bound (10, after the 30),
	// and W1 crossed it too (8 recording functions left, 5 checking the stop and 3 not): a number on a population that shrinks with every migration fails the next
	// PR that crosses it, so it is gone and the predicate stays, which still fails on a scan that sees nothing (0 and 0) or only one kind.
	if recording == 0 || withCheck == 0 || len(withoutCheck) == 0 {
		t.Fatalf("the scan found %d functions that record an event, %d that check the stop and %d that do not; it must see both kinds, so it is not seeing the engine",
			recording, withCheck, len(withoutCheck))
	}
	var unclassified, stale []string
	for k := range withoutCheck {
		if _, ok := recordedEventSitesWithoutOwnStopCheck[k]; !ok {
			unclassified = append(unclassified, k)
		}
	}
	for k := range recordedEventSitesWithoutOwnStopCheck {
		if !withoutCheck[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("these functions record an event and neither call stopBeforeNewWork nor appear in recordedEventSitesWithoutOwnStopCheck: %v.\n"+
			"A failed write of the event must stop the guest (cleat#4160): check the stop before the work, or list the function with the reason it does not need to.", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("these exceptions no longer apply (the function now checks the stop, or no longer records an event): %v", stale)
	}
}
