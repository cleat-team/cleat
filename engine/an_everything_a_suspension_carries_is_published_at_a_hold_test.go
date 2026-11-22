package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// cleat#4162. A run that is HELD through a wait does not return to the worker, and the worker persists what an execution RETURNS only when it returns: at a suspension or at the end.
// So anything the suspending `return` carries is invisible to every client for the whole hold unless parkOrSuspend publishes it first (publishForHold). That is how query state was
// missed: nothing connected the list of things a suspension carries to the list of things a hold publishes, so a hold could be added without ever asking.
//
// This ties the two lists together STRUCTURALLY. It reads the `return` statements in executor.go that return a SuspendResult and collects every `session.<field>` they carry, then
// reads publishForHold in park.go and collects every `s.<field>` it handles. A field the suspension carries must be handled by the hold, or be mapped to a field that handles it, or be
// exempted below WITH A REASON. A new field added to the suspending return and not here fails this test and names the field.
//
// WHAT THIS CANNOT SEE, said so it is not mistaken for coverage: a value carried through a LOCAL variable (executor.go's replay path returns `strippedHistory`, a local, not
// `session.history`) is not a selector and is not collected; the history field is collected through the other return. And it checks that the hold REFERENCES a field, not that it handles
// it correctly: the behaviour is pinned by TestAHold... and the worker-level test.

// carriedBy maps a field the suspension carries to the field the hold handles it through, when they are not the same field.
var carriedBy = map[string]string{
	// The events a segment recorded. They are written step by step unless the engine runs with noPerStepFlush, in which case they sit in `unflushed`, and publishForHold REFUSES the
	// hold while any are there. So the field the hold handles is `unflushed`.
	"history": "unflushed",
}

// exemptFromHold is a field the suspension carries that a hold deliberately does not publish, and why. An entry that is no longer carried by the suspension is itself a failure.
var exemptFromHold = map[string]string{
	"deferrals": "the defer table is rebuilt from the recorded events when the run is replayed (a defer is an event: durable-defer records one), and a held run is still assigned to this worker, " +
		"so a crash during the hold replays it from the store; the table is carried on a suspension for the failure path that runs a killed run's defers, which a hold does not reach",
}

// holdBookkeeping are the session fields publishForHold touches to do its work or to report, which are not state it publishes. They must NOT count as "handled": the function appends to
// holdPublishFailures, so a field of that name carried by a suspension would otherwise be waved through by the function's own bookkeeping (measured: a mutant that added
// session.holdPublishFailures to the suspending return left this guard green until these were excluded).
var holdBookkeeping = map[string]bool{"engine": true, "holdPublishFailures": true, "publishedQueryState": true}

// suspensionCarries returns the set of `session.<field>` selectors in every return statement of src that returns something named susResult, and how many such statements it found.
func suspensionCarries(t *testing.T, src string) (map[string]bool, int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "executor.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the executor: %v", err)
	}
	carried := map[string]bool{}
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		isSuspension := false
		for _, r := range ret.Results {
			if id, ok := r.(*ast.Ident); ok && id.Name == "susResult" {
				isSuspension = true
			}
		}
		if !isSuspension {
			return true
		}
		found++
		for _, r := range ret.Results {
			ast.Inspect(r, func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "session" {
						carried[sel.Sel.Name] = true
					}
				}
				return true
			})
		}
		return true
	})
	return carried, found
}

// holdHandles returns the set of `s.<field>` selectors the named function of src references.
func holdHandles(t *testing.T, src, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "park.go", src, 0)
	if err != nil {
		t.Fatalf("parsing park.go: %v", err)
	}
	handled := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
					handled[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return handled
}

// unaccountedFor lists the carried fields that neither the hold handles, nor a mapping routes to a field it handles, nor an exemption covers; and the exemptions that are stale.
func unaccountedFor(carried, handled map[string]bool, via, exempt map[string]string) (missing, stale []string) {
	for f := range carried {
		switch {
		case handled[f]:
		case via[f] != "" && handled[via[f]]:
		case exempt[f] != "":
		default:
			missing = append(missing, f)
		}
	}
	for f, why := range exempt {
		if !carried[f] {
			stale = append(stale, f)
		}
		if strings.TrimSpace(why) == "" {
			stale = append(stale, f+" (no reason)")
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	return
}

func TestEverythingASuspensionCarriesIsPublishedAtAHold(t *testing.T) {
	exec, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	park, err := os.ReadFile("park.go")
	if err != nil {
		t.Fatal(err)
	}
	carried, found := suspensionCarries(t, string(exec))
	if found < 2 {
		t.Fatalf("UNMEASURED: found %d suspending return statement(s) in executor.go, want at least 2 (Execute and the replay path). The guard read nothing, so a pass would mean nothing.", found)
	}
	for _, must := range []string{"queryState", "history", "deferrals"} {
		if !carried[must] {
			t.Fatalf("UNMEASURED: the suspending return no longer carries session.%s (carried: %v): either the field was removed, which this test's tables must follow, or the scan broke", must, carried)
		}
	}
	handled := holdHandles(t, string(park), "publishForHold")
	for f := range holdBookkeeping {
		delete(handled, f)
	}
	if !handled["queryState"] {
		t.Fatalf("UNMEASURED: publishForHold references no s.queryState (handled: %v): the scan of park.go read the wrong function or the hold no longer publishes query state", handled)
	}

	missing, stale := unaccountedFor(carried, handled, carriedBy, exemptFromHold)
	if len(missing) > 0 {
		t.Errorf("the suspending return carries session.%s but publishForHold neither handles it nor is it exempted: a run HELD through a wait never returns, so it would be invisible for the whole hold. "+
			"Publish it in publishForHold (park.go), or add it to exemptFromHold with the reason it need not be.", strings.Join(missing, ", session."))
	}
	if len(stale) > 0 {
		t.Errorf("exemptFromHold has entries the suspending return no longer carries, or with no reason: %v", stale)
	}
}

// The guard's own known-positive: a field added to the suspending return and not handled must fail, a field that IS handled must not, and a stale exemption must fail. Without these a
// guard that matched nothing would pass forever.
func TestTheHoldGuardFailsOnAFieldItDoesNotHandle(t *testing.T) {
	const exec = `package engine
func f() {
	susResult := 0
	return "", g(session.history), susResult, session.deferrals, session.queryState, session.progress, nil
}`
	const park = `package engine
func publishForHold() { _ = s.queryState; _ = s.unflushed; s.holdPublishFailures = nil; _ = s.engine; _ = s.publishedQueryState }`
	carried, found := suspensionCarries(t, exec)
	if found != 1 || !carried["progress"] || !carried["queryState"] || !carried["history"] {
		t.Fatalf("PRECONDITION FAILED: the scan of the synthetic source found %d return(s) and carried=%v", found, carried)
	}
	handled := holdHandles(t, park, "publishForHold")
	for f := range holdBookkeeping {
		delete(handled, f)
	}
	missing, stale := unaccountedFor(carried, handled, carriedBy, exemptFromHold)
	if len(missing) != 1 || missing[0] != "progress" {
		t.Errorf("missing = %v, want exactly [progress]: a field the suspension carries and the hold does not handle was not caught", missing)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none for the real exemption table", stale)
	}
	// A stale exemption (the field is no longer carried) is a failure too.
	_, stale = unaccountedFor(map[string]bool{"queryState": true}, map[string]bool{"queryState": true}, carriedBy, map[string]string{"gone": "no longer carried"})
	if len(stale) != 1 {
		t.Errorf("stale = %v, want the one exemption for a field that is not carried", stale)
	}
	// An exemption with no reason is refused.
	_, stale = unaccountedFor(map[string]bool{"x": true}, map[string]bool{}, nil, map[string]string{"x": " "})
	if len(stale) == 0 {
		t.Errorf("an exemption with no reason was accepted")
	}
}

// A suspension that carries a field NAMED like one publishForHold only touches for bookkeeping must still fail: the hold appends to holdPublishFailures, and that is not publishing it.
func TestTheHoldGuardIsNotFooledByTheHoldsOwnBookkeeping(t *testing.T) {
	const exec = `package engine
func f() {
	susResult := 0
	return "", g(session.history), susResult, session.deferrals, session.queryState, session.holdPublishFailures, nil
}`
	const park = `package engine
func publishForHold() { _ = s.queryState; _ = s.unflushed; s.holdPublishFailures = append(s.holdPublishFailures, "x") }`
	carried, _ := suspensionCarries(t, exec)
	handled := holdHandles(t, park, "publishForHold")
	if !handled["holdPublishFailures"] {
		t.Fatalf("PRECONDITION FAILED: the synthetic hold does not reference holdPublishFailures, so this measured nothing")
	}
	for f := range holdBookkeeping {
		delete(handled, f)
	}
	missing, _ := unaccountedFor(carried, handled, carriedBy, exemptFromHold)
	if len(missing) != 1 || missing[0] != "holdPublishFailures" {
		t.Errorf("missing = %v, want [holdPublishFailures]: the hold's own bookkeeping reference was taken for publishing it", missing)
	}
}
