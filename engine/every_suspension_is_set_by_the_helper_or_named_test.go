package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// parkOrSuspend is meant to be the only place a WAIT sets suspendErr (cleat#4152, W2b). Most waits have not moved onto it yet, so this guard
// states the gap and lets it only shrink: every assignment to suspendErr in production code is either in suspendFor (the helper's own) or in
// the table below with its exact count. A NEW assignment fails (a wait that decides for itself is the thing the helper exists to stop), and so
// does a table entry that is higher than the code (a call that moved must lower or delete its line), so the table is the migration's checklist
// and cannot go stale in either direction.
//
// Two of the entries are not waits and never will move: a shutdown stop and a failed-event-write stop suspend the run, they do not wait for
// anything, so there is nothing to park.
var notYetOnTheParkHelper = map[string]struct {
	count int
	why   string
}{
	"children.go:AwaitChild":                 {1, "a wait; moves with the child waits (W3)"},
	"children.go:AwaitAnyChild":              {1, "a wait; moves with the child waits (W3)"},
	"children.go:freshAwaitAllChildren":      {1, "a wait; moves with the child waits (W3)"},
	"promises.go:AwaitPromise":               {3, "a wait; moves with AwaitPromise (W4)"},
	"signaller.go:DurableAwaitSignals":       {2, "a wait; moves with await_signals (W5)"},
	"scope.go:freshSetScope":                 {2, "a wait for a lock scope; moves with SetScope (WS-cleat-2)"},
	"lifecycle.go:ContinueAsNew":             {2, "a terminal call, not a wait; moves with the terminal group (W1)"},
	"lifecycle.go:ContinueAsNewWithVersion":  {2, "a terminal call, not a wait; moves with the terminal group (W1)"},
	"durablecalls.go:freshCall":              {1, "a shutdown stop, not a wait: nothing to park"},
	"lifecycle.go:stopAfterFailedEventWrite": {1, "a failed-event-write stop, not a wait: nothing to park"},
}

func TestEverySuspensionIsSetByTheHelperOrIsNamedAsNotYetMoved(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "suspendErr" {
						found[f+":"+fn.Name.Name]++
					}
				}
				return true
			})
		}
	}

	// CONTROL: the scan sees the helper's own assignment. If it did not, "nothing else assigns" would be true of a scan that sees nothing.
	if found["park.go:suspendFor"] != 1 {
		t.Fatalf("the scan did not find the helper's own assignment in park.go:suspendFor (found %d); it cannot be trusted to find others", found["park.go:suspendFor"])
	}
	delete(found, "park.go:suspendFor")

	var problems []string
	for site, n := range found {
		want, listed := notYetOnTheParkHelper[site]
		switch {
		case !listed:
			problems = append(problems, site+": sets suspendErr itself ("+strconv.Itoa(n)+" site(s)); a wait goes through parkOrSuspend, and a call that is not a wait must be added to notYetOnTheParkHelper with its reason")
		case n != want.count:
			problems = append(problems, site+": the table says "+strconv.Itoa(want.count)+" site(s) ("+want.why+") but the code has "+strconv.Itoa(n)+"; if a site moved onto the helper, lower the count or delete the entry")
		}
	}
	for site, want := range notYetOnTheParkHelper {
		if found[site] == 0 {
			problems = append(problems, site+": listed ("+want.why+") but sets suspendErr nowhere; delete the entry")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
