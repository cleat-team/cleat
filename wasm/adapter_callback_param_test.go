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
// widened the scan to cover hostWrapperDefs, it was clean ON THE COVERED LIST
// -- worse than being out of scope, because the list now asserted it had been
// checked. Found by WS-3 (session_01... , reported 2026-09-07) reading the
// merged guard, not by the guard.
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
var callbackParamsNotPassedToTheHost = map[string]string{
	"DurableCallWithHeartbeat.onProgress": "cleat#854: onProgress cannot be invoked across the ABI -- " +
		"the guest is suspended inside the cleat_call_heartbeat import for the whole call, " +
		"so there is no moment at which the host could run guest code. Rust's equivalent " +
		"already takes no callback. Removing the parameter is a breaking change to a public " +
		"SDK signature, which is the user's call, so it is recorded here rather than made. " +
		"The host-side heartbeat itself works and is not in question.",

	"DurableCallTypedWithHeartbeat.onProgress": "cleat#854: the typed wrapper forwards onProgress to " +
		"DurableCallWithHeartbeat, which drops it, so it is equally inert -- one hop further out. " +
		"Listed separately because the resolution has to cover both signatures: if the parameter is " +
		"removed it is removed from both, and if it is wired both need wiring. Whoever fixes #854 by " +
		"deleting only the entry above will see this one fail, which is the point.",
}

type callbackSite struct {
	name   string
	params []adapterParam
	body   string
}

func generatedCallbackSites(t *testing.T) []callbackSite {
	t.Helper()
	// BOTH maps. adapterDefs holds the closures that call an import directly;
	// hostWrapperDefs holds the typed wrappers that call those. They are
	// separate types with separate body fields, and scanning only the first
	// would have left DurableCallTypedWithHeartbeat unchecked.
	var sites []callbackSite
	for name, def := range adapterDefs {
		sites = append(sites, callbackSite{name, def.Params,
			strings.Join(append(append([]string{}, def.PreStmts...), def.ResultStmts...), "\n")})
	}
	for name, def := range hostWrapperDefs {
		sites = append(sites, callbackSite{name, def.Params, strings.Join(def.Body, "\n")})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].name < sites[j].name })
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
func reachedCallbackParams(t *testing.T, sites []callbackSite) map[string]bool {
	t.Helper()

	type edge struct {
		callee string
		index  int
	}
	direct := map[string]bool{}     // "Fn.param" -> body does something else with it
	forwards := map[string][]edge{} // "Fn.param" -> parameters it is handed to
	known := map[string]callbackSite{}
	for _, s := range sites {
		known[s.name] = s
	}

	// The generated typed wrappers call the adapters through a "host_" prefix.
	resolve := func(callee string) (string, bool) {
		for _, n := range []string{callee, strings.TrimPrefix(callee, "host_")} {
			if _, ok := known[n]; ok {
				return n, true
			}
		}
		return "", false
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
			t.Fatalf("%s: generated body does not parse, so it cannot be checked: %v\n\n%s",
				site.name, err, src)
		}
		body := file.Decls[0].(*ast.FuncDecl).Body

		for _, p := range site.params {
			if !strings.HasPrefix(p.Type, "func(") {
				continue
			}
			key := site.name + "." + p.Name

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
						forwards[key] = append(forwards[key], edge{target, i})
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
				if reached[e.callee+"."+callee.params[e.index].Name] {
					reached[key] = true
					changed = true
					break
				}
			}
		}
	}
	return reached
}

func TestEveryCallbackParameterReachesTheGeneratedBody(t *testing.T) {
	sites := generatedCallbackSites(t)
	reached := reachedCallbackParams(t, sites)

	for _, site := range sites {
		for _, p := range site.params {
			if !strings.HasPrefix(p.Type, "func(") {
				continue
			}
			key := site.name + "." + p.Name
			why, exempt := callbackParamsNotPassedToTheHost[key]

			switch {
			case reached[key] && exempt:
				t.Errorf("%s DOES reach the host, but is still listed in "+
					"callbackParamsNotPassedToTheHost.\n\nDelete the entry: an exemption that no "+
					"longer describes a violation is a grant covering whatever is added next.\n"+
					"Reason on file: %s", key, why)
			case !reached[key] && !exempt:
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
		} else if def, ok := hostWrapperDefs[fn]; ok {
			params = def.Params
		} else {
			t.Errorf("callbackParamsNotPassedToTheHost names %q, which is in neither "+
				"adapterDefs nor hostWrapperDefs. Delete the entry.", key)
			continue
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
		{"Sink", []adapterParam{{"a", "string"}, sink}, `_ = a`},
		// Mentions it, only to hand it to the one that drops it. This is
		// DurableCallTypedWithHeartbeat, and #856 called it clean.
		{"Forwarder", []adapterParam{{"a", "string"}, sink}, `host_Sink(a, onProgress)`},
		// Hands it to an import: reaches the host, which is the good case.
		{"Reacher", []adapterParam{{"a", "string"}, sink}, `cleatSomeImport(a, onProgress)`},
		// Calls it outright: also good.
		{"Caller", []adapterParam{{"a", "string"}, sink}, `onProgress(a)`},
		// Names it in a comment and nowhere else. A text search reads this as
		// a use; an AST walk does not.
		{"Commenter", []adapterParam{{"a", "string"}, sink}, "// onProgress is deliberately not called yet\n_ = a"},
	}

	reached := reachedCallbackParams(t, sites)

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
		if got := reached[tc.site+".onProgress"]; got != tc.want {
			t.Errorf("%s.onProgress: reached=%v, want %v -- %s", tc.site, got, tc.want, tc.why)
		}
	}

	if t.Failed() {
		t.Log(fmt.Sprintf("reached map: %v", reached))
	}
}
