// Package entrypointstructresult is the fixture for
// TestVetGo_EntryPointMustReturnString.
//
// It is a workflow that vet accepted and `cleat build` could not compile: the
// entry point returns a pointer to a struct, and the generated exports file
// assumes a string. Three shipped examples were in this state
// (IMPROVEMENT-PLAN 3.228).
package entrypointstructresult

import "github.com/cleat-team/cleat/cleat"

// Result is what the entry point wrongly returns.
type Result struct {
	OK bool `json:"ok"`
}

// Workflow returns *Result, which no language SDK can express as a WASM
// result. The parameter being a struct is fine and deliberately included, so
// the fixture pins that only the RESULT is rejected.
func Workflow(h cleat.HostCalls, input Result) (*Result, error) {
	if _, err := h.DurableCall("s", "op", "{}"); err != nil {
		return nil, err
	}
	return &Result{OK: true}, nil
}
