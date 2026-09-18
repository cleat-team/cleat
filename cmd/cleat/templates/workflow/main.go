//go:build ignore

package main

import (
	"github.com/cleat-team/cleat/cleat"
)

// @cleatEntry(name="process")
//
// The ANNOTATION, not a //go:wasmexport directive. cleat build reads this and
// generates the export in gen_wasm_memory.go; writing //go:wasmexport here
// too produced "symbol process redeclared", and Go's own toolchain rejects the
// signature anyway ("unsupported parameter type cleat.HostCalls"). basic and
// agent have always used the annotation. cleat#1888.
func Process(h cleat.HostCalls, input string) (string, error) {
	h.LogKV("workflow_started", "input", input)

	// TODO: add your workflow logic here
	// Examples:
	//   result, err := h.DurableCall("llm", "chat", `{"provider":"anthropic","model":"claude-sonnet-4-6","messages":[...]}`)
	//   signal := h.AwaitSignals([]string{"approve", "reject"}, 15*time.Minute)
	//   h.SetQueryState("status", "completed")

	return "processed: " + input, nil
}

// No func main here on purpose: `cleat build` generates gen_main_stub.go,
// which declares it. A main in this file collides with the generated one --
// "other declaration of main" -- and is why this template did not build
// (cleat#1888). basic and agent have never declared one.
