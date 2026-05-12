package main

import (
	"github.com/rcownie/cleat/cleat"
)

var h cleat.HostCalls

//go:wasmexport process
func Process(h cleat.HostCalls, input string) (string, error) {
	h.LogKV("workflow_started", "input", input)

	// TODO: add your workflow logic here
	// Examples:
	//   result, err := h.DurableCall("llm", "chat", `{"provider":"anthropic","model":"claude-sonnet-4-6","messages":[...]}`)
	//   signal := h.AwaitSignals([]string{"approve", "reject"}, 15*time.Minute)
	//   h.SetQueryState("status", "completed")

	return "processed: " + input, nil
}

func main() {}
