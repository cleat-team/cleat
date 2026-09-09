package wasm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A function-typed adapter parameter that never reaches the host is a callback
// the workflow author supplies and the engine never calls.
//
// It compiles: an unused PARAMETER is legal Go, unlike an unused variable. So
// nothing in the toolchain objects, the SDK's doc comment goes on promising the
// callback, and the only way to find out is to write a workflow that would
// notice. Measured for DurableCallWithHeartbeat on 2026-09-06 -- a workflow
// whose onProgress sent to a fixture service reported call=1, progressCallback=0.
//
// This is the shape #806 fixed for RunDetached, in that commit's own words:
//
//	It used to take a closure, which cannot cross the ABI -- so it worked
//	under localdev and cleattest, which populate the field directly, and
//	silently did nothing in every compiled workflow.
//
// The existing closure-coverage guard (closure_hostcall_coverage_test.go) does
// not catch this and is not meant to: it asks whether a method's closure field
// is WIRED, and these are. A wired adapter can still ignore a parameter, which
// is a different question and needs this different check.
//
// # Why this asks about reachability and not about reference
//
// The first version of this guard tested `strings.Contains(body, param)` --
// "is the parameter mentioned". That is the wrong question by exactly one hop,
// and the tree contains the hop. DurableCallTypedWithHeartbeat's body is
//
//	resp, err := host_DurableCallWithHeartbeat(service, operation, string(reqJSON), heartbeatInterval, onProgress)
//
// which mentions onProgress while handing it to the one function known to drop
// it. The mention-test called that clean and, because the same commit had just
// widened the scan to cover a second table, it was clean ON THE COVERED LIST
// -- worse than being out of scope, because the list now asserted it had been
// checked. Found by WS-3 reading the merged guard, not by the guard.
//
// So: a parameter is REACHED if the body does anything with it other than
// forward it into another generated function whose matching parameter is
// itself not reached. Forwarding to anything NOT in these maps -- an import,
// in particular, which is where the ABI boundary is -- counts as reached,
// because that is the call this guard exists to confirm happens.
//
// The analysis is an AST walk rather than a text search, which is also why a
// mention of a parameter inside a comment can no longer read as a use: a
// comment does not parse to an identifier.

// callbackParamsNotPassedToTheHost are function-typed adapter parameters that
// deliberately do not reach the host, keyed by "Function.parameter", with the
// issue that will resolve each.
//
// Keyed by BOTH names on purpose. Keyed by function alone, a second callback
// parameter added to an already-listed function would be exempt on arrival,
// covered by an entry written about a different parameter -- the same
// one-grain-too-coarse mistake in a different place.
//
// A list that may only SHRINK, checked in both directions below. An entry that
// stops describing a violation is a standing exemption covering nothing, which
// is how an allowlist becomes a hole for whatever is added next.
//
// EMPTY, and that is the goal state rather than an oversight. Its only two
// entries were DurableCallWithHeartbeat.onProgress and the typed wrapper's,
// both removed in cleat#854 by deleting the parameter itself. An empty list
// means every function-typed adapter parameter in the tree reaches the host.
var callbackParamsNotPassedToTheHost = map[string]string{}

type callbackSite struct {
	origin string // which map: the two share three names, see the collision test
	name   string
	params []adapterParam
	body   string
}

// qualified is the key this analysis is indexed by. It carries the map name,
// which is now redundant -- adapterDefs is the only table since cleat#1003 --
// and is kept because it costs nothing and the synthetic sites in
// TestTheGuardRefusesAnAmbiguousForward still rely on two origins existing.
// It mattered when there were two overlapping namespaces: DurableSleep, Now
// and AcquireLock were each defined in both with different parameters, and
// keyed by bare name two different functions' parameters would
// merge into one entry, and a forward would resolve to whichever of the two Go
// happened to iterate last -- a guard whose answer changes between runs.
func (s callbackSite) qualified(param string) string {
	return s.origin + ":" + s.name + "." + param
}

func generatedCallbackSites(t *testing.T) []callbackSite {
	t.Helper()
	// adapterDefs is now the only table a build reads. There was a second,
	// hostWrapperDefs, and this scan covered both -- deliberately, because
	// scanning only adapterDefs would have left DurableCallTypedWithHeartbeat
	// unchecked. That table and its emitters were deleted (cleat#1003) once it
	// was established that nothing generated a host_ call, the transformer did
	// not rewrite to one, and OutputFiles had no field such a file could be
	// written into. The wrapper-only entries went with it, so there is nothing
	// left for a second loop to find.
	var sites []callbackSite
	for name, def := range adapterDefs {
		sites = append(sites, callbackSite{"adapterDefs", name, def.Params,
			strings.Join(append(append([]string{}, def.PreStmts...), def.ResultStmts...), "\n")})
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].name != sites[j].name {
			return sites[i].name < sites[j].name
		}
		return sites[i].origin < sites[j].origin
	})
	return sites
}

// reachedCallbackParams answers, for every function-typed parameter of every
// site, whether it reaches something outside this set of generated bodies.
//
// Least fixpoint: a parameter starts unreached and only ever becomes reached,
// either by a direct use or by being forwarded into a parameter already known
// to be reached. Forwarding cycles therefore settle at unreached, which is the
// correct answer -- a value passed in a ring and never used never reaches the
// host.
func reachedCallbackParams(sites []callbackSite) (map[string]bool, error) {
	type edge struct {
		callee string
		index  int
	}
	direct := map[string]bool{}     // qualified key -> body does something else with it
	forwards := map[string][]edge{} // qualified key -> parameters it is handed to
	known := map[string]callbackSite{}
	byName := map[string][]callbackSite{}
	for _, s := range sites {
		known[s.origin+":"+s.name] = s
		byName[s.name] = append(byName[s.name], s)
	}

	// Resolve a called name to the site it means, or report that it is not one
	// of ours -- which is the good case, since an import is the ABI crossing
	// this guard exists to confirm.
	//
	// "host_X" is how a generated wrapper names an adapter, so that prefix
	// picks adapterDefs specifically. A bare name that exists in both maps is
	// genuinely ambiguous and fails rather than guessing: guessing here would
	// resolve a forward to SOMETHING rather than nothing, and this analysis
	// treats "forwarded somewhere unknown" as reached, so a wrong guess fails
	// toward the flattering answer.
	// Set when resolve() meets a name it cannot disambiguate. Recorded rather
	// than fatal so that the guard's own regression test can assert on it: a
	// branch that kills the process cannot be exercised by a test in the same
	// process, and an unexercised branch of a guard is exactly what this file
	// exists to be suspicious of.
	var ambiguous error

	resolve := func(callee string) (callbackSite, bool) {
		if bare := strings.TrimPrefix(callee, "host_"); bare != callee {
			site, ok := known["adapterDefs:"+bare]
			return site, ok
		}
		candidates := byName[callee]
		switch len(candidates) {
		case 0:
			return callbackSite{}, false
		case 1:
			return candidates[0], true
		default:
			if ambiguous == nil {
				ambiguous = fmt.Errorf("a generated body calls %q, which is defined in two "+
					"tables with different parameters. This analysis "+
					"cannot tell which one the call means, and guessing fails toward "+
					"\"reached\". Give the call the \"host_\" prefix if it means the adapter, "+
					"or qualify it here.", callee)
			}
			return callbackSite{}, false
		}
	}

	for _, site := range sites {
		var decls []string
		for _, p := range site.params {
			decls = append(decls, p.Name+" "+p.Type)
		}
		src := "package p\nfunc _guard(" + strings.Join(decls, ", ") + ") {\n" + site.body + "\n}\n"
		file, err := parser.ParseFile(token.NewFileSet(), "site.go", src, parser.SkipObjectResolution)
		if err != nil {
			// Not a skip. A body this guard cannot parse is a body it cannot
			// check, and silently passing it is the failure mode being fixed.
			return nil, fmt.Errorf("%s: generated body does not parse, so it cannot be "+
				"checked: %w\n\n%s", site.name, err, src)
		}
		body := file.Decls[0].(*ast.FuncDecl).Body

		for _, p := range site.params {
			if !strings.HasPrefix(p.Type, "func(") {
				continue
			}
			key := site.qualified(p.Name)

			// Every mention of the parameter inside the body...
			total := 0
			ast.Inspect(body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == p.Name {
					total++
				}
				return true
			})

			// ...minus the ones that are only handing it onward to another
			// generated function. Anything left over is a real use.
			forwarded := 0
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					callee = fn.Name
				case *ast.SelectorExpr:
					callee = fn.Sel.Name
				}
				target, isGenerated := resolve(callee)
				if !isGenerated {
					return true // an import or the runtime: reaching it IS the point
				}
				for i, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == p.Name {
						forwarded++
						forwards[key] = append(forwards[key], edge{target.origin + ":" + target.name, i})
					}
				}
				return true
			})

			if total > forwarded {
				direct[key] = true
			}
		}
	}

	reached := map[string]bool{}
	for k, v := range direct {
		reached[k] = v
	}
	for changed := true; changed; {
		changed = false
		for key, edges := range forwards {
			if reached[key] {
				continue
			}
			for _, e := range edges {
				callee := known[e.callee]
				if e.index >= len(callee.params) {
					continue // arity mismatch: not a forward we can follow
				}
				if reached[callee.qualified(callee.params[e.index].Name)] {
					reached[key] = true
					changed = true
					break
				}
			}
		}
	}
	if ambiguous != nil {
		return nil, ambiguous
	}
	return reached, nil
}

func TestEveryCallbackParameterReachesTheGeneratedBody(t *testing.T) {
	sites := generatedCallbackSites(t)
	reached, err := reachedCallbackParams(sites)
	if err != nil {
		t.Fatal(err)
	}

	for _, site := range sites {
		for _, p := range site.params {
			if !strings.HasPrefix(p.Type, "func(") {
				continue
			}
			// Two keys, deliberately. Reachability is indexed by map as well as
			// name, because the two maps share three names. The exemption list
			// is keyed by bare "Function.parameter" because that is what a
			// person writes and reads -- safe only while no shared name carries
			// a callback, which TestTheTwoMapsDoNotShareANameWithACallback is
			// what enforces.
			key := site.name + "." + p.Name
			why, exempt := callbackParamsNotPassedToTheHost[key]

			switch {
			case reached[site.qualified(p.Name)] && exempt:
				t.Errorf("%s DOES reach the host, but is still listed in "+
					"callbackParamsNotPassedToTheHost.\n\nDelete the entry: an exemption that no "+
					"longer describes a violation is a grant covering whatever is added next.\n"+
					"Reason on file: %s", key, why)
			case !reached[site.qualified(p.Name)] && !exempt:
				t.Errorf("%s takes a callback parameter %q of type %s that never reaches the host, "+
					"so a workflow that supplies one is handed a callback the engine will never "+
					"invoke.\n\nThe body may still MENTION it -- forwarding it to another generated "+
					"function that drops it does not count, which is the whole point of this check. "+
					"An unused parameter is legal Go, so this compiles and the SDK doc comment goes "+
					"on promising the callback. Either pass it to the host, or remove it from the "+
					"signature the way #806 did for RunDetached.",
					site.name, p.Name, p.Type)
			}
		}
	}
}

// TestTheCallbackExemptionsNameRealAdapters is the second direction for the
// half the loop above cannot see: an entry for an adapter that no longer
// exists, or for a parameter it no longer has, matches nothing and would sit
// here unnoticed.
func TestTheCallbackExemptionsNameRealAdapters(t *testing.T) {
	for key := range callbackParamsNotPassedToTheHost {
		fn, param, ok := strings.Cut(key, ".")
		if !ok {
			t.Errorf("callbackParamsNotPassedToTheHost key %q is not in Function.parameter form.", key)
			continue
		}
		var params []adapterParam
		if def, ok := adapterDefs[fn]; ok {
			params = def.Params
		}
		found := false
		for _, p := range params {
			if p.Name == param && strings.HasPrefix(p.Type, "func(") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("callbackParamsNotPassedToTheHost names %q, but %s has no function-typed "+
				"parameter called %q any more. Delete the entry.", key, fn, param)
		}
	}
}

// TestTheGuardSeesThroughOneHopOfForwarding is the guard's own regression test,
// and it fails against the version of this file merged as #856.
//
// Written with synthetic sites rather than the real maps so that fixing #854
// -- which will change what the real maps say -- cannot quietly delete the
// coverage. The shapes are the ones that actually occur.
func TestTheGuardSeesThroughOneHopOfForwarding(t *testing.T) {
	sink := adapterParam{"onProgress", "func(string)"}

	sites := []callbackSite{
		// Drops it: mentions it nowhere.
		{"adapterDefs", "Sink", []adapterParam{{"a", "string"}, sink}, `_ = a`},
		// Mentions it, only to hand it to the one that drops it. This is
		// DurableCallTypedWithHeartbeat, and #856 called it clean.
		{"adapterDefs", "Forwarder", []adapterParam{{"a", "string"}, sink}, `host_Sink(a, onProgress)`},
		// Hands it to an import: reaches the host, which is the good case.
		{"adapterDefs", "Reacher", []adapterParam{{"a", "string"}, sink}, `cleatSomeImport(a, onProgress)`},
		// Calls it outright: also good.
		{"adapterDefs", "Caller", []adapterParam{{"a", "string"}, sink}, `onProgress(a)`},
		// Names it in a comment and nowhere else. A text search reads this as
		// a use; an AST walk does not.
		{"adapterDefs", "Commenter", []adapterParam{{"a", "string"}, sink}, "// onProgress is deliberately not called yet\n_ = a"},
	}

	reached, err := reachedCallbackParams(sites)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		site string
		want bool
		why  string
	}{
		{"Sink", false, "never mentions the parameter"},
		{"Forwarder", false, "forwards it only to a function that drops it -- the case #856 missed"},
		{"Reacher", true, "passes it to an import, which is the ABI crossing this guard wants"},
		{"Caller", true, "invokes it directly"},
		{"Commenter", false, "mentions it in a comment, which is not a use"},
	} {
		if got := reached["adapterDefs:"+tc.site+".onProgress"]; got != tc.want {
			t.Errorf("%s.onProgress: reached=%v, want %v -- %s", tc.site, got, tc.want, tc.why)
		}
	}

	if t.Failed() {
		t.Logf("reached map: %v", reached)
	}
}

// TestTheGuardRefusesAnAmbiguousForward is the known-positive for resolve()'s
// ambiguity branch, which no real body exercises today.
//
// This branch guarded a real overlap until cleat#1003: adapterDefs and
// hostWrapperDefs shared AcquireLock, DurableSleep and Now with DIFFERENT
// parameter lists on each side. One table remains, so the overlap cannot occur
// today and the synthetic sites below are the only thing exercising it. Kept
// rather than deleted because a second table is a plausible future and the
// branch that refuses to guess has
// never run. A guard path that has never executed is a claim, not a check, and
// this file's whole subject is the difference.
//
// The refusal has to be a refusal and not a guess, because the direction is not
// symmetric: this analysis treats "forwarded somewhere I do not recognise" as
// REACHED, since an unrecognised callee is normally an import and reaching an
// import is the thing being confirmed. So a wrong guess about a shared name
// silently produces the flattering answer -- the parameter is reported as
// reaching the host when it may do nothing of the kind. Worse, which of the two
// a guess picked would depend on Go's randomized map iteration, so the guard's
// answer would differ between runs of the same tree.
//
// Synthetic sites, for the same reason TestTheGuardSeesThroughOneHopOfForwarding
// uses them: this must keep testing the branch after the real maps change.
func TestTheGuardRefusesAnAmbiguousForward(t *testing.T) {
	sink := adapterParam{"onProgress", "func(string)"}

	// "Shared" stands in for Now/DurableSleep/AcquireLock: one name, two
	// definitions, different parameters. Note the two disagree on arity, which
	// is what makes a guess actively wrong rather than merely unprincipled.
	sites := []callbackSite{
		{"adapterDefs", "Shared", []adapterParam{{"a", "string"}, sink}, `_ = a`},
		{"syntheticDefs", "Shared", []adapterParam{sink}, `_ = onProgress`},
		{"syntheticDefs", "Forwarder", []adapterParam{{"a", "string"}, sink}, `Shared(a, onProgress)`},
	}

	_, err := reachedCallbackParams(sites)
	if err == nil {
		t.Fatal("a callback forwarded into a name defined in BOTH maps was resolved without " +
			"complaint. resolve() must refuse rather than guess: an unrecognised callee counts " +
			"as reached, so guessing wrong reports a dead callback as live, and which way it " +
			"guesses depends on map iteration order.")
	}
	if !strings.Contains(err.Error(), "Shared") {
		t.Errorf("the refusal does not name the ambiguous callee, so it cannot be acted on: %v", err)
	}

	// Second direction: the SAME forward is unambiguous once the call says
	// which map it means, and then it must resolve rather than refuse.
	// "host_Shared" names the adapter, whose onProgress is dropped -- so the
	// forwarder's parameter is correctly unreached rather than erroring.
	sites[2].body = `host_Shared(a, onProgress)`
	reached, err := reachedCallbackParams(sites)
	if err != nil {
		t.Fatalf("host_-qualified forward should resolve, not refuse: %v", err)
	}
	if reached["syntheticDefs:Forwarder.onProgress"] {
		t.Error("Forwarder.onProgress forwards only into adapterDefs:Shared, which drops it, " +
			"so it must not be reported as reaching the host.")
	}
}
