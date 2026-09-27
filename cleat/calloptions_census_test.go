package cleat

import (
	"reflect"
	"sort"
	"testing"
)

// TestEveryCallOptionsFieldIsHonouredSomewhere is the guard cleat#1006 asked
// for: it fails when a field is ADDED to CallOptions, so that whoever adds one
// has to say where it is honoured before it ships.
//
// The defect it exists to prevent is not "a field is wrong" but "a field is
// ACCEPTED AND IGNORED". CallOptions.Timeout was accepted, documented, and
// inert on every path a workflow can run on -- measured 2026-09-13, one slow
// call against one short timeout, each row with a control proving the delay was
// really in the path:
//
//	compiled WASM (cleat build --target go)   inert
//	cleat/cleattest (unit-test harness)       inert
//	cleat/localdev (local dev runner)         inert
//	hand-built HostCalls, WithOptions nil     works  <- cleat/runtime_test.go
//
// Only the last row is reachable from a test, which is exactly why the field's
// own tests were green for as long as it existed. A caller who set a 50ms
// timeout on a 2s call believed they had a bound and did not have one.
//
// So this test does NOT check that a field works -- reflection cannot -- it
// checks that the SET of fields is the set somebody decided on. Adding one
// breaks this test, and the honoured column below is where the answer goes.
func TestEveryCallOptionsFieldIsHonouredSomewhere(t *testing.T) {
	// Where each field is actually read. Keep this honest: "no reader" is a
	// legitimate entry and is the whole point of the census.
	honoured := map[string]string{
		"Retry": "cleat/runtime.go DurableCallWithOptions (host import cleat_call_retry, " +
			"or the SDK loop when the host refuses the policy) and cleat/localdev",
	}

	var got []string
	for i := 0; i < reflect.TypeOf(CallOptions{}).NumField(); i++ {
		got = append(got, reflect.TypeOf(CallOptions{}).Field(i).Name)
	}
	sort.Strings(got)

	var want []string
	for name := range honoured {
		want = append(want, name)
	}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CallOptions fields are %v, the census says %v.\n\n"+
			"If you ADDED a field: add it to the honoured map above and name the "+
			"file and line that reads it. If nothing reads it yet, the field is "+
			"not ready to ship -- a caller who sets it will believe it took "+
			"effect, which is cleat#1006 exactly. Note that the three paths a "+
			"workflow can really run on are compiled WASM, cleat/cleattest and "+
			"cleat/localdev, and the first of those reaches CallOptions only if "+
			"wasm/adapterDefs has a DurableCallWithOptions entry -- it does not, "+
			"so today a compiled workflow's options are read by the SDK fallback "+
			"in this package and nowhere else.\n\n"+
			"If you REMOVED a field: drop its entry above.", got, want)
	}
}
