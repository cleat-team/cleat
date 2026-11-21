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
	"children.go:var awaitChildStep":              {1, "a wait for a child: the park helper has no child wake kind, so the spec sets suspendErr itself (W3a)"},
	"children.go:var awaitAllChildrenStep":        {1, "a wait for every child in a list: as await_child, no child wake kind on the park helper (W3a)"},
	"children.go:var awaitAnyChildStep":           {1, "a wait for any of a set of children: as the other two child awaits, no child wake kind on the park helper (W3b)"},
	"scope.go:freshSetScope":                      {2, "a wait for a lock scope; moves with SetScope (WS-cleat-2)"},
	"lifecycle.go:var continueAsNewStep":          {2, "a terminal call, not a wait: it ENDS the run (Reason continue_as_new), on its replay half and its fresh half (W1)"},
	"lifecycle.go:var continueAsNewVersionedStep": {2, "a terminal call, not a wait: the same, carrying a version (W1)"},
	"durablecalls.go:freshCall":                   {1, "a shutdown stop, not a wait: nothing to park"},
	"lifecycle.go:stopAfterFailedEventWrite":      {1, "a failed-event-write stop, not a wait: nothing to park"},
}

// suspendSites counts the assignments to suspendErr in one parsed file, keyed by where they sit: "file:Func" for a function or method (including
// any literal nested in it), and "file:var Name" for a package-level var, which is where a step spec's Fresh literal lives. A scan of function
// bodies alone cannot see the second kind: a suspension set inside `var x = stepSpec{Fresh: func(c *stepCtx) int64 {...}}` belongs to no
// FuncDecl, so it would read as a site that had moved to the helper when it is neither (WS-cleat-2, #4237).
func suspendSites(fname string, file *ast.File) map[string]int {
	found := map[string]int{}
	count := func(key string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "suspendErr" {
					found[key]++
				}
			}
			return true
		})
	}
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				count(fname+":"+d.Name.Name, d.Body)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, sp := range d.Specs {
				vs := sp.(*ast.ValueSpec)
				for _, v := range vs.Values {
					count(fname+":var "+vs.Names[0].Name, v)
				}
			}
		}
	}
	return found
}

// The scan has to see a suspension wherever it is set. Each case is a source the scan reads and the key it must report; the first is
// WS-cleat-2's known-positive, which the function-bodies-only scan returned nothing for.
func TestTheSuspensionScanSeesAnAssignmentWhereverItSits(t *testing.T) {
	for name, c := range map[string]struct {
		src  string
		want map[string]int
	}{
		"in a literal in a package-level spec": {
			"package p\nvar zz = stepSpec{Fresh: func(c *stepCtx) int64 { c.s.suspendErr = nil; return 0 }}\n",
			map[string]int{"x.go:var zz": 1},
		},
		"in a method": {
			"package p\nfunc (s *execSession) M() { s.suspendErr = nil }\n",
			map[string]int{"x.go:M": 1},
		},
		"in a literal nested in a method (attributed to the method)": {
			"package p\nfunc (s *execSession) M() { f := func() { s.suspendErr = nil }; f() }\n",
			map[string]int{"x.go:M": 1},
		},
		"two in one var": {
			"package p\nvar zz = stepSpec{Fresh: func(c *stepCtx) int64 { c.s.suspendErr = nil; c.s.suspendErr = nil; return 0 }}\n",
			map[string]int{"x.go:var zz": 2},
		},
		// CONTROLS: a read, and an assignment to some other field, are not suspensions.
		"a read is not an assignment": {
			"package p\nfunc (s *execSession) M() bool { return s.suspendErr != nil }\n",
			map[string]int{},
		},
		"another field is not suspendErr": {
			"package p\nvar zz = stepSpec{Fresh: func(c *stepCtx) int64 { c.s.eventWriteFailure = nil; return 0 }}\n",
			map[string]int{},
		},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", c.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := suspendSites("x.go", file)
		if len(got) != len(c.want) {
			t.Errorf("%s: scan = %v, want %v", name, got, c.want)
			continue
		}
		for k, n := range c.want {
			if got[k] != n {
				t.Errorf("%s: scan = %v, want %v", name, got, c.want)
			}
		}
	}
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
		for k, n := range suspendSites(f, file) {
			found[k] += n
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
