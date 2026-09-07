package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every WorkflowStore method must be reachable from production code, or carry a
// reason saying why not.
//
// # The shape this catches
//
// Five defects found on 2026-09-05 share one form: a complete, tested READ path
// in front of a WRITE path that nothing reaches (cleat#769).
//
//	PollAndClaimSignal          PollSignal is live; nothing consumes  -> signals never consumed
//	SetRoutingRule              PickVersionByRouting runs every start -> A/B routing can never fire
//	SetWorkflowTag              nothing consults tags                 -> tag family inert both ways
//	DeleteDeadLetteredWorkflows n/a                                   -> dead-letter table only grows
//	ValidateVersion             ListVersions has no deprecated filter -> deprecation unenforced
//
// Each passes its tests, because the tests call the store method directly.
// Nothing asserted that production reaches it.
//
// The A/B case shows why this is worse than an unimplemented feature.
// `server.go` runs `PickVersionByRouting` on every workflow start and logs
// "A/B routing applied" when it fires. `SetRoutingRule` had no production
// caller, so no rule could exist, so the branch was dead and the log line
// unreachable. **The read side's presence is what stops anyone asking whether
// the write side exists.** An absent feature fails loudly at the point of use;
// this one silently does the default thing forever.
//
// # Why this is an AST walk and not a grep
//
// Two failure modes, both of which this repo has paid for, and both of which
// disappear when the question is asked of a parse tree rather than of text:
//
//   - A grep cannot tell a call from a sentence about one. Comments and STRING
//     LITERALS both match; the literals are the nastier half, because stripping
//     comments does not reach them. An `*ast.SelectorExpr` is neither.
//   - `\.Method\(` misses method VALUES. cleat#769 records the false positive
//     it produced: `VerifyWorkflowEvents` is wired at
//     `cmd/cleat-worker/setup.go`, passed to `engine.WithWorkflowEventVerifier`
//     without a call paren. A call-syntax pattern cannot see callbacks -- and
//     callbacks are where verifiers, hooks and injected policies live, so the
//     omission is not random: it is systematically the category carrying safety
//     behaviour. `Sel.Name` matches both forms because the AST does not
//     distinguish them.
func TestEveryStoreMethodIsReachableFromProduction(t *testing.T) {
	methods := workflowStoreMethodNames(t)
	if len(methods) < 40 {
		t.Fatalf("parsed only %d WorkflowStore methods; there were 100+ on 2026-09-07. "+
			"A parse that finds almost nothing passes vacuously.", len(methods))
	}

	prod, testOnly, _, _ := storeMethodReferences(t, methods)

	var unreachable, onlyTests []string
	for _, m := range methods {
		if storeReachabilityExemptions[m] != "" || storeUnreachedBaseline[m] {
			continue
		}
		if prod[m] {
			continue
		}
		if testOnly[m] {
			onlyTests = append(onlyTests, m)
			continue
		}
		unreachable = append(unreachable, m)
	}
	sort.Strings(unreachable)
	sort.Strings(onlyTests)

	for _, m := range unreachable {
		t.Errorf("WorkflowStore.%s is referenced by no production code.\n\n"+
			"A store method nothing reaches is a write path behind a read path that works -- "+
			"the shape cleat#769 is about. Wire it, delete it, or add it to "+
			"storeReachabilityExemptions with the reason it is deliberately unreached.", m)
	}
	for _, m := range onlyTests {
		t.Errorf("WorkflowStore.%s is referenced ONLY from tests.\n\n"+
			"That is a finding, not an exemption: a method exercised only by tests that call "+
			"it directly is exactly what every defect in cleat#769 looked like from inside "+
			"the test suite.", m)
	}
	t.Logf("%d WorkflowStore methods, %d exempt, %d in the baseline",
		len(methods), len(storeReachabilityExemptions), len(storeUnreachedBaseline))
}

// storeUnreachedBaseline is the debt this guard found when it was introduced.
//
// A baseline, NOT an exemption, and the difference is the point. An exemption
// says "deliberately unreached, here is why". These are not deliberate -- they
// are cleat#769's finding, enumerated so the guard can be live against
// everything else while they are dealt with one at a time.
//
// IT MAY ONLY SHRINK. TestTheUnreachedBaselineOnlyShrinks fails when a
// baselined method becomes reachable, which forces its removal rather than
// letting the entry sit there asserting something that has stopped being true.
// Nothing may be added without wiring or deleting the method instead.
//
// Four of these are cleat#769's original five. The fifth, PollAndClaimSignal, is
// absent because it was fixed in IMPROVEMENT-PLAN 3.215 -- replaced by
// ConsumeSignal and removed from the interface. Its name still appears in five
// comments, which is why the issue's shell approximation cannot be the guard:
// a grep for the name finds the retraction that records its removal.
//
// The other six the issue did not list. That is the argument for a guard rather
// than a sweep: the same shape, found by asking the question mechanically.
var storeUnreachedBaseline = map[string]bool{
	// cleat#769's original findings.
	"DeleteDeadLetteredWorkflows": true,
	"SetRoutingRule":              true,
	"SetWorkflowTag":              true,
	"ValidateVersion":             true,

	// Found by this guard. Each is the same shape: a write or read path with
	// no production caller, whose siblings are live.
	"GetWorkflowTag":        true,
	"GetWorkflowTags":       true,
	"LoadWorkflowConfig":    true,
	"RemoveRoutingRule":     true,
	"RemoveWorkflowTag":     true,
	"StreamEventHistory":    true,
	"UpdateScheduleNextRun": true,
}

// TestTheUnreachedBaselineOnlyShrinks is the baseline's second direction.
//
// Without it the baseline is a list of assertions nothing checks: wire a method
// up and its entry stays, still claiming the method is unreached. The guard
// would then be quietly weaker than its own record of itself -- which is the
// failure mode every exemption in this repo has eventually had.
func TestTheUnreachedBaselineOnlyShrinks(t *testing.T) {
	methods := workflowStoreMethodNames(t)
	known := map[string]bool{}
	for _, m := range methods {
		known[m] = true
	}
	prod, _, _, _ := storeMethodReferences(t, methods)

	for m := range storeUnreachedBaseline {
		if !known[m] {
			t.Errorf("storeUnreachedBaseline has %q, which is not a WorkflowStore method. "+
				"If it was deleted, drop the entry -- the baseline is debt, and a deleted "+
				"method is debt paid.", m)
			continue
		}
		if prod[m] {
			t.Errorf("WorkflowStore.%s is in storeUnreachedBaseline but production now "+
				"references it. Remove the entry: the baseline may only shrink, and an "+
				"entry that no longer describes anything is how a guard learns to be quiet.", m)
		}
	}
	t.Logf("baseline: %d methods still unreached", len(storeUnreachedBaseline))
}

// storeReachabilityExemptions maps a method to the reason it is deliberately
// unreached by production code.
//
// A reason, not a marker. cleat#769: "an escape hatch that takes a bare
// //nolint-style marker will absorb exactly the cases this is meant to catch."
// TestEveryStoreReachabilityExemptionIsStillNeeded checks the other direction --
// an exemption for a method that HAS become reachable is a lie the guard would
// otherwise keep telling.
var storeReachabilityExemptions = map[string]string{}

// workflowStoreMethodNames reads the method set off the WorkflowStore interface
// declaration.
//
// From the AST rather than a grep over the file: the interface body contains
// doc comments that mention other methods by name, and several of those
// sentences would parse as declarations to anything line-oriented.
func workflowStoreMethodNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store_interface.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing store_interface.go: %v", err)
	}

	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "WorkflowStore" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return false
		}
		for _, m := range it.Methods.List {
			for _, id := range m.Names {
				names = append(names, id.Name)
			}
		}
		return false
	})
	sort.Strings(names)
	return names
}

// storeMethodReferences reports, for each method, whether any production file
// and whether any test file names it as a selector.
//
// Files come from `git ls-files`, not a filesystem walk: this repo keeps
// worktrees under .claude/, and a walk descends into what is effectively a
// second copy of the tree -- which would let a reference in a scratch checkout
// vouch for the real one.
func storeMethodReferences(t *testing.T, methods []string) (
	prod, testOnly map[string]bool,
	referrers map[string]map[string]bool,
	prodNamesUsed map[string]bool,
) {
	t.Helper()
	want := map[string]bool{}
	for _, m := range methods {
		want[m] = true
	}

	root := repoRootForReachability(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < 200 {
		t.Fatalf("git ls-files returned %d .go files; expected hundreds. "+
			"A file list that is nearly empty makes every method look unreachable, "+
			"which would fail loudly -- but a partial one would make an arbitrary "+
			"subset look unreachable, which reads as a finding.", len(files))
	}

	prod, testOnly = map[string]bool{}, map[string]bool{}
	referrers = map[string]map[string]bool{}
	prodNamesUsed = map[string]bool{}
	fset := token.NewFileSet()
	scanned := 0
	for _, rel := range files {
		if skipForReachability(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue // a file listed but unreadable is not this test's business
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			continue // testdata and generated fixtures need not parse
		}
		scanned++
		isTest := strings.HasSuffix(rel, "_test.go")

		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			enclosing := ""
			if isFunc {
				enclosing = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.SelectorExpr:
					// Sel.Name covers a call (x.M()) and a method value (x.M)
					// alike: both are SelectorExpr, and the difference is the
					// enclosing node. That is the whole reason this is an AST
					// walk -- see the doc comment on the test.
					if want[v.Sel.Name] {
						if isTest {
							testOnly[v.Sel.Name] = true
						} else {
							prod[v.Sel.Name] = true
							if enclosing != "" && enclosing != v.Sel.Name {
								if referrers[v.Sel.Name] == nil {
									referrers[v.Sel.Name] = map[string]bool{}
								}
								referrers[v.Sel.Name][enclosing] = true
							}
						}
					}
					if !isTest && enclosing != v.Sel.Name {
						prodNamesUsed[v.Sel.Name] = true
					}
				case *ast.Ident:
					if !isTest && v.Name != enclosing {
						prodNamesUsed[v.Name] = true
					}
				}
				return true
			})
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d parseable Go files; the skip list is too broad", scanned)
	}
	// A method seen in production is not "test only" whatever the tests do.
	for m := range prod {
		delete(testOnly, m)
	}
	return prod, testOnly, referrers, prodNamesUsed
}

// skipForReachability drops the files that DEFINE the surface rather than
// consume it. Counting those would make every method look reachable, which is
// the direction that fails silently.
func skipForReachability(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "engine/testutil/"):
		// A test harness. Its references are test references, and it has no
		// _test.go suffix to say so.
		return true
	case rel == "engine/store_interface.go":
		// The declaration itself.
		return true
	case rel == "engine/sharded_store.go":
		// Pure delegation: ShardedStore implements every method by routing to a
		// shard, so it references all of them and would vouch for all of them.
		return true
	case strings.HasPrefix(rel, ".claude/"):
		return true
	}
	return false
}

func repoRootForReachability(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locating the repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestEveryStoreReachabilityExemptionIsStillNeeded is the second direction.
//
// An exemption is prose, and prose does not fail when the thing it describes
// stops being true. Without this, wiring an exempt method up leaves the
// exemption in place, still asserting the method is unreached -- and the next
// method to go dark inherits a guard that has learned to be quiet.
func TestEveryStoreReachabilityExemptionIsStillNeeded(t *testing.T) {
	methods := workflowStoreMethodNames(t)
	known := map[string]bool{}
	for _, m := range methods {
		known[m] = true
	}
	prod, _, _, _ := storeMethodReferences(t, methods)

	for m, why := range storeReachabilityExemptions {
		if !known[m] {
			t.Errorf("storeReachabilityExemptions has %q, which is not a WorkflowStore method. "+
				"A renamed or deleted method leaves an exemption covering nothing.\n\nreason on file: %s",
				m, why)
			continue
		}
		if prod[m] {
			t.Errorf("WorkflowStore.%s is exempt as unreached, but production now references it. "+
				"Remove the exemption.\n\nreason on file: %s", m, why)
		}
		if strings.TrimSpace(why) == "" || len(why) < 40 {
			t.Errorf("the exemption for %s gives %d characters of reason. A marker without a "+
				"justification absorbs exactly the cases this guard exists to catch (cleat#769).",
				m, len(why))
		}
	}
}

// TestNoStoreMethodIsReachableOnlyThroughDeadCode closes the hole that
// TestEveryStoreMethodIsReachableFromProduction had on the day it merged.
//
// That test asks "does production reference this method". It does not ask
// whether the referencing FUNCTION is itself reached, so a method referenced
// only from a function nothing calls reads as green. cleat#869 names the shape:
// "in scope and falsely clean is worse than out of scope" -- a scan that covers
// the file and reaches the wrong conclusion is worse than one that admits it
// does not look there, because the coverage is what stops anyone checking.
//
// It found two on its first run, and one of them was a real defect:
//
//	GetCompactionCandidates  only in compactionLoop, which launchLoop never
//	                         starts -- so history compaction has NEVER run
//	                         (cleat#877). Every other piece is present: the
//	                         --compaction-threshold flag, the store method on
//	                         all three dialects, CompactWorkflowHistory, a fuzz
//	                         test, and the loop's own unit tests.
//	ClaimWorkflow            only in waitForDB, called from tests alone. The
//	                         real claim path uses ClaimWorkflows (plural); this
//	                         one is a DB-connectivity probe.
//
// # One level, not full reachability
//
// This asks whether the referring function's NAME is used anywhere in
// production -- called, taken as a value, or passed to a loop launcher. It does
// not compute transitive reachability from main; scripts/check-unreachable-main.sh
// does that for package main, and duplicating it here would be a second
// implementation of a hard analysis rather than a cheap check of a different
// question.
//
// One level is enough for the shape this is about: a whole subsystem wired to
// nothing. It would not catch a chain of two dead functions, which is a real
// limit and is why this carries a baseline rather than claiming completeness.
func TestNoStoreMethodIsReachableOnlyThroughDeadCode(t *testing.T) {
	methods := workflowStoreMethodNames(t)
	_, _, referrers, prodNamesUsed := storeMethodReferences(t, methods)

	if len(prodNamesUsed) < 500 {
		t.Fatalf("collected only %d production identifiers; the scan is broken. "+
			"An identifier set that is nearly empty makes every referring function "+
			"look dead, which reads as a wall of findings rather than as a fault.",
			len(prodNamesUsed))
	}

	var dead []string
	for m, fns := range referrers {
		if storeDeadReferrerBaseline[m] {
			continue
		}
		live := false
		for fn := range fns {
			if prodNamesUsed[fn] {
				live = true
				break
			}
		}
		if live {
			continue
		}
		names := make([]string, 0, len(fns))
		for fn := range fns {
			names = append(names, fn)
		}
		sort.Strings(names)
		dead = append(dead, m+" (only in: "+strings.Join(names, ", ")+")")
	}
	sort.Strings(dead)

	for _, d := range dead {
		t.Errorf("WorkflowStore.%s is referenced only from production functions that nothing "+
			"itself references.\n\n"+
			"The method looks reached and is not. This is the shape cleat#869 describes: a scan "+
			"that covers the file and reaches the wrong conclusion is worse than one that admits "+
			"it does not look there. Wire the referring function up, delete it, or baseline it "+
			"with an issue.", d)
	}
	t.Logf("%d methods have production referrers; %d baselined as dead-referrer",
		len(referrers), len(storeDeadReferrerBaseline))
}

// storeDeadReferrerBaseline is the debt found when the check above was added.
//
// Shrink-only, like storeUnreachedBaseline, and for the same reason: these are
// findings held open under an issue, not decisions.
var storeDeadReferrerBaseline = map[string]bool{
	// cleat#877: compactionLoop is never launched, so history compaction has
	// never run. Removing this entry is part of fixing that.
	"GetCompactionCandidates": true,

	// waitForDB is called only from tests. Its ClaimWorkflow call is a
	// DB-connectivity probe, not the claim path -- that is ClaimWorkflows.
	// Tracked with cleat#877; either waitForDB is wired into startup or it goes.
	"ClaimWorkflow": true,
}
