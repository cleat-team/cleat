package wasm

import "testing"

// TestSagaAddStepCallIsADurableHelper pins the entry that makes
// Saga.AddStepCall visible to the three layers that decide what a workflow does.
//
// WHY A TEST AND NOT JUST THE TABLE. Without it the failure is silent in the
// worst way: the workflow builds, reports "0 host functions used", produces a
// module importing no cleat_call, and dies at RUN time on its first step with
// "the HostCalls runtime was not initialized" -- a message that names the entry
// point and points away from the binding. That is cleat#1005's symptom reached
// from the opposite direction, and cleat#1005 is why this file's sibling table
// is a slice rather than a map.
//
// The end-to-end proof is testdata/sagaparameterised, which a `cleat build`
// turns into a module whose imports can be inspected. This test guards the much
// cheaper thing: that the row exists at all and names the right import, so
// deleting it fails here rather than three layers away.
func TestSagaAddStepCallIsADurableHelper(t *testing.T) {
	imports, ok := sdkHelperImports["Saga.AddStepCall"]
	if !ok {
		t.Fatal("Saga.AddStepCall has no sdkHelperImports row.\n\n" +
			"It builds its DurableCall closures inside the SDK, so no layer that " +
			"scans WORKFLOW code for HostCalls methods can see them. Without this " +
			"row the module imports no cleat_call and fails at run time, not build " +
			"time. cleat#1131.")
	}
	want := "cleat_call"
	found := false
	for _, im := range imports {
		if im == want {
			found = true
		}
	}
	if !found {
		t.Errorf("Saga.AddStepCall maps to %v, which does not include %q -- "+
			"the host function its generated closures actually call", imports, want)
	}
}

// TestSDKHelperKeyUnwrapsPointerReceivers is the control on the key format.
// Saga methods are declared on *Saga, so a key builder that did not unwrap the
// pointer would return "" for every real call site and the table would never be
// consulted -- a lookup that always misses, which reads exactly like a table
// with no rows.
func TestSDKHelperKeyUnwrapsPointerReceivers(t *testing.T) {
	for key := range sdkHelperImports {
		if key == "" {
			t.Error("sdkHelperImports has an empty key; sdkHelperKey returns \"\" for " +
				"unresolvable receivers and such a row could never be hit")
		}
		// A key must be Type.Method, not *Type.Method: sdkHelperKey unwraps the
		// pointer, so a row written with a star can never match.
		if len(key) > 0 && key[0] == '*' {
			t.Errorf("sdkHelperImports key %q starts with '*'; sdkHelperKey unwraps "+
				"pointer receivers, so this row is unreachable", key)
		}
	}
}
