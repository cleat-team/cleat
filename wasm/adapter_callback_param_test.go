package wasm

import (
	"strings"
	"testing"
)

// A function-typed adapter parameter that the generated body never references
// is a callback the workflow author supplies and the engine never calls.
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

// callbackParamsNotPassedToTheHost are function-typed adapter parameters the
// generated body deliberately does not use, with the issue that will resolve
// each.
//
// A list that may only SHRINK, checked in both directions below. An entry that
// stops describing a violation is a standing exemption covering nothing, which
// is how an allowlist becomes a hole for whatever is added next.
var callbackParamsNotPassedToTheHost = map[string]string{
	"DurableCallWithHeartbeat": "cleat#854: onProgress cannot be invoked across the ABI -- " +
		"the guest is suspended inside the cleat_call_heartbeat import for the whole call, " +
		"so there is no moment at which the host could run guest code. Rust's equivalent " +
		"already takes no callback. Removing the parameter is a breaking change to a public " +
		"SDK signature, which is the user's call, so it is recorded here rather than made. " +
		"The host-side heartbeat itself works and is not in question.",
}

func TestEveryCallbackParameterReachesTheGeneratedBody(t *testing.T) {
	// BOTH maps. adapterDefs holds the closures that call an import directly;
	// hostWrapperDefs holds the typed wrappers that call those. They are
	// separate types with separate body fields, and scanning only the first
	// would have left DurableCallTypedWithHeartbeat unchecked -- which is
	// exactly the half-covered scan this file exists to prevent elsewhere.
	// The second-direction test below is what caught that: an exemption named
	// an entry the first map does not contain.
	type callbackSite struct {
		name   string
		params []adapterParam
		body   string
	}
	var sites []callbackSite
	for name, def := range adapterDefs {
		sites = append(sites, callbackSite{name, def.Params,
			strings.Join(append(append([]string{}, def.PreStmts...), def.ResultStmts...), "\n")})
	}
	for name, def := range hostWrapperDefs {
		sites = append(sites, callbackSite{name, def.Params, strings.Join(def.Body, "\n")})
	}

	for _, site := range sites {
		name := site.name
		for _, p := range site.params {
			if !strings.HasPrefix(p.Type, "func(") {
				continue
			}

			used := strings.Contains(site.body, p.Name)

			why, exempt := callbackParamsNotPassedToTheHost[name]

			switch {
			case used && exempt:
				t.Errorf("%s.%s IS referenced by the generated body, but %s is still listed in "+
					"callbackParamsNotPassedToTheHost.\n\nDelete the entry: an exemption that no "+
					"longer describes a violation is a grant covering whatever is added next.\n"+
					"Reason on file: %s", name, p.Name, name, why)
			case !used && !exempt:
				t.Errorf("%s takes a callback parameter %q of type %s and the generated body never "+
					"references it, so a workflow that supplies one is handed a callback the engine "+
					"will never invoke.\n\nAn unused PARAMETER is legal Go, so this compiles and the "+
					"SDK doc comment goes on promising the callback. Either pass it to the host, or "+
					"remove it from the signature the way #806 did for RunDetached.",
					name, p.Name, p.Type)
			}
		}
	}
}

// TestTheCallbackExemptionsNameRealAdapters is the second direction for the
// half the loop above cannot see: an entry for an adapter that no longer exists,
// or that has no callback parameter at all, matches nothing and would sit here
// unnoticed.
func TestTheCallbackExemptionsNameRealAdapters(t *testing.T) {
	for name := range callbackParamsNotPassedToTheHost {
		var params []adapterParam
		if def, ok := adapterDefs[name]; ok {
			params = def.Params
		} else if def, ok := hostWrapperDefs[name]; ok {
			params = def.Params
		} else {
			t.Errorf("callbackParamsNotPassedToTheHost names %q, which is in neither "+
				"adapterDefs nor hostWrapperDefs. Delete the entry.", name)
			continue
		}
		hasCallback := false
		for _, p := range params {
			if strings.HasPrefix(p.Type, "func(") {
				hasCallback = true
				break
			}
		}
		if !hasCallback {
			t.Errorf("callbackParamsNotPassedToTheHost names %q, which has no function-typed "+
				"parameter any more. Delete the entry.", name)
		}
	}
}
