package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
// This half covers the fields of `session` only. What a suspension makes the WORKER write (status, next_wake_at) and the fields of SuspendResult are the second guard further down,
// TestEverythingTheWorkerWritesAtASuspensionIsAccountedForAtAHold, added after cleat-review3 found that this one could not see them.
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

// THE SECOND HALF OF THE SAME QUESTION, which the guard above cannot see (cleat-review3, #4363): it reads only `session.<field>` inside the suspending returns. A suspension also
// makes the WORKER write things, and those come from the SuspendResult it is handed and from a literal, not from the session: in cmd/cleat-worker/setup.go, under
// `if suspended != nil`, `finalStatus = "ready"` and `nextWakeAt = suspended.SuspendUntil`. A held run does neither: it stays `running` with no wake time.
//
// That is a deliberate choice, option (a) in docs/design/resident-suspended-runs.md section 4.2 ("Stay running"), and it was recorded nowhere a change could trip over it. These two tables
// are where it is recorded. The test fails when the worker starts to write a new value at a suspension, or SuspendResult gains a field, that is not named here with the reason a
// hold may leave it out: so the decision is asked about again by whoever adds the next one, and is not a thing a reader has to rediscover.
//
// This reads cmd/cleat-worker/setup.go through "..": `go test` caches on files opened INSIDE the package directory, so a local run after editing setup.go alone can print (cached).
// CI passes -count=1 to every package, so it is sound there; run with -count=1 locally.

// suspensionAssigns names each variable the worker assigns inside `if suspended != nil` in the function that finalizes a segment, and why a hold leaves it unwritten.
var suspensionAssigns = map[string]string{
	"finalStatus": "the suspension writes status 'ready' (waiting to be claimed); a held run stays 'running' on this worker (option (a), design doc 4.2), which is the truth for it: it holds a claim, " +
		"is in w.inflight, and the heartbeat keeps the reaper off it. This is the one user-visible difference a hold makes, and it is why design invariant 4 carries an exception for it",
	"nextWakeAt": "the suspension writes next_wake_at from SuspendUntil so another worker can claim the run at the wake; a held run has no wake time in the store because it wakes in-process. If its worker " +
		"dies the reaper recovers it after about 47 s and it replays, as for any running run (design doc 4.2)",
}

// suspendResultFields names each field of SuspendResult and what a hold does about it. A field the worker reads is said to be read.
var suspendResultFields = map[string]string{
	"History":              "carried through the other return as the stripped history and covered by the first guard's history -> unflushed mapping; the events themselves are written per step",
	"SuspendUntil":         "READ by the worker as nextWakeAt: see suspensionAssigns[nextWakeAt]",
	"Reason":               "READ by the worker to choose the continue_as_new branch and to log; a hold is only ever of a timer or a signal wait (the kind refusal), never of continue_as_new",
	"NewInput":             "READ by the worker for continue_as_new only, which is not a wait and is never held",
	"NewVersion":           "READ by the worker for continue_as_new only, which is not a wait and is never held",
	"Deferrals":            "not read from SuspendResult by the worker (the table is returned separately); see exemptFromHold[deferrals]",
	"ContinueAsNewHandled": "continue_as_new only, which is not a wait and is never held",
	"NewRunID":             "continue_as_new only, which is not a wait and is never held",
}

// suspendResultFieldsOf returns the field names of the struct type SuspendResult in src.
func suspendResultFieldsOf(t *testing.T, src string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", src, 0)
	if err != nil {
		t.Fatalf("parsing types.go: %v", err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "SuspendResult" {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			for _, fld := range st.Fields.List {
				for _, nm := range fld.Names {
					out = append(out, nm.Name)
				}
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// whatTheWorkerDoesAtASuspension returns the variables assigned inside any `if suspended != nil { ... }` in src, the `suspended.<Field>` selectors read anywhere in src, and how many
// such if statements it found.
func whatTheWorkerDoesAtASuspension(t *testing.T, src string) (assigned, read map[string]bool, ifs int) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "setup.go", src, 0)
	if err != nil {
		t.Fatalf("parsing setup.go: %v", err)
	}
	assigned, read = map[string]bool{}, map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "suspended" {
				read[x.Sel.Name] = true
			}
		case *ast.IfStmt:
			be, ok := x.Cond.(*ast.BinaryExpr)
			if !ok || be.Op != token.NEQ {
				return true
			}
			lhs, ok1 := be.X.(*ast.Ident)
			rhs, ok2 := be.Y.(*ast.Ident)
			if !ok1 || !ok2 || lhs.Name != "suspended" || rhs.Name != "nil" {
				return true
			}
			ifs++
			ast.Inspect(x.Body, func(m ast.Node) bool {
				if as, ok := m.(*ast.AssignStmt); ok {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name != "_" { // `_ = x` assigns nothing anywhere
							assigned[id.Name] = true
						}
					}
				}
				return true
			})
		}
		return true
	})
	return assigned, read, ifs
}

// accountedFor reports what is in `have` but absent from `table` (new, unaccounted), and what is in `table` but absent from `have` or carries no reason (stale).
func accountedFor(have map[string]bool, table map[string]string) (unaccounted, stale []string) {
	for k := range have {
		if _, ok := table[k]; !ok {
			unaccounted = append(unaccounted, k)
		}
	}
	for k, why := range table {
		if !have[k] || strings.TrimSpace(why) == "" {
			stale = append(stale, k)
		}
	}
	sort.Strings(unaccounted)
	sort.Strings(stale)
	return
}

func TestEverythingTheWorkerWritesAtASuspensionIsAccountedForAtAHold(t *testing.T) {
	setup, err := os.ReadFile(filepath.Join("..", "cmd", "cleat-worker", "setup.go"))
	if err != nil {
		t.Fatalf("UNMEASURED: reading setup.go: %v", err)
	}
	types, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("UNMEASURED: reading types.go: %v", err)
	}
	assigned, read, ifs := whatTheWorkerDoesAtASuspension(t, string(setup))
	if ifs == 0 {
		t.Fatalf("UNMEASURED: found no `if suspended != nil` in setup.go, so the guard read nothing and a pass would mean nothing")
	}
	for _, must := range []string{"finalStatus", "nextWakeAt"} {
		if !assigned[must] {
			t.Fatalf("UNMEASURED: the worker no longer assigns %s under `if suspended != nil` (found %v): either it was renamed, which this test's tables must follow, or the scan broke", must, assigned)
		}
	}
	fields := suspendResultFieldsOf(t, string(types))
	if len(fields) < 5 {
		t.Fatalf("UNMEASURED: read only %d SuspendResult fields from types.go (%v)", len(fields), fields)
	}

	if un, stale := accountedFor(assigned, suspensionAssigns); len(un) > 0 || len(stale) > 0 {
		t.Errorf("the worker assigns %v at a suspension that suspensionAssigns does not account for, and suspensionAssigns has stale or reasonless entries %v. A run HELD through a wait never reaches "+
			"the suspension, so a value written only there is never written for it. Decide whether a hold must write it, and record the decision in suspensionAssigns.", un, stale)
	}
	haveFields := map[string]bool{}
	for _, f := range fields {
		haveFields[f] = true
	}
	if un, stale := accountedFor(haveFields, suspendResultFields); len(un) > 0 || len(stale) > 0 {
		t.Errorf("SuspendResult has fields %v that suspendResultFields does not account for, and suspendResultFields has stale or reasonless entries %v. Say what a hold does about each.", un, stale)
	}
	for f := range read {
		if !haveFields[f] {
			t.Errorf("setup.go reads suspended.%s but SuspendResult has no such field: the scan matched something that is not the struct", f)
		}
	}
}

// The second guard's known-positives. Without them a guard that read nothing, or whose tables everything fell into, would pass forever.
func TestTheSuspensionGuardFailsOnAValueItDoesNotAccountFor(t *testing.T) {
	const worker = `package main
func finalize() {
	if suspended != nil {
		finalStatus = "ready"
		nextWakeAt = suspended.SuspendUntil
		retryAt = suspended.Reason
		_ = suspended.Reason
	}
	_ = suspended.NewInput
}`
	assigned, read, ifs := whatTheWorkerDoesAtASuspension(t, worker)
	if ifs != 1 || !assigned["retryAt"] || !assigned["finalStatus"] || !read["Reason"] || !read["NewInput"] {
		t.Fatalf("PRECONDITION FAILED: the scan of the synthetic worker found %d if(s), assigned=%v read=%v", ifs, assigned, read)
	}
	if assigned["_"] {
		t.Errorf("the blank identifier was counted as a value the worker writes at a suspension")
	}
	if un, _ := accountedFor(assigned, suspensionAssigns); len(un) != 1 || un[0] != "retryAt" {
		t.Errorf("unaccounted = %v, want exactly [retryAt]: a new value written at a suspension was not caught", un)
	}
	// A new field on SuspendResult is caught the same way.
	const types = `package engine
type SuspendResult struct {
	History []int
	SuspendUntil int
	Reason string
	NewInput string
	NewVersion int
	Deferrals int
	ContinueAsNewHandled bool
	NewRunID string
	WakeHint string
}`
	fields := suspendResultFieldsOf(t, types)
	have := map[string]bool{}
	for _, f := range fields {
		have[f] = true
	}
	if un, _ := accountedFor(have, suspendResultFields); len(un) != 1 || un[0] != "WakeHint" {
		t.Errorf("unaccounted = %v, want exactly [WakeHint]: a new SuspendResult field was not caught", un)
	}
	// A stale or reasonless entry is a failure too.
	if _, stale := accountedFor(map[string]bool{"finalStatus": true}, map[string]string{"finalStatus": "x", "gone": "was removed"}); len(stale) != 1 || stale[0] != "gone" {
		t.Errorf("stale = %v, want [gone]", stale)
	}
	if _, stale := accountedFor(map[string]bool{"a": true}, map[string]string{"a": "  "}); len(stale) != 1 {
		t.Errorf("a reasonless entry was accepted: stale = %v", stale)
	}
	// And a worker with no `if suspended != nil` at all reports zero, which the real test turns into UNMEASURED rather than a pass.
	if _, _, n := whatTheWorkerDoesAtASuspension(t, "package main\nfunc f() {}"); n != 0 {
		t.Errorf("found %d if statements in a source that has none", n)
	}
}
