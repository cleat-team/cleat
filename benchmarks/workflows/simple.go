// Package workflows defines benchmark workflow functions for the cleat vs
// Temporal vs DBOS comparison. Each workflow is a standalone Go function
// that takes a cleat.HostCalls and a typed input, and returns a typed output.
//
// These workflows are designed to be framework-agnostic: the same business
// logic can be adapted for Temporal (via the Go SDK) or DBOS (via its SDK)
// to produce an apples-to-apples comparison.
package workflows

import (
	"github.com/rcownie/cleat/durable"
)

// SimpleInput configures the simple sequential workflow.
type SimpleInput struct {
	// Steps is the number of sequential durable calls to make.
	Steps int `json:"steps"`
}

// SimpleOutput is the result of the simple workflow.
type SimpleOutput struct {
	Done bool `json:"done"`
}

// SimpleWorkflow executes a configurable number of sequential durable API
// calls. Each call goes through the full HostCalls plumbing (determinism
// tracking, event recording, etc.) making it a clean micro-benchmark for
// framework overhead per step.
//
// Equivalent Temporal workflow: a stub.ExecuteWorkflow with N sequential
// Activity.Execute calls. Equivalent DBOS workflow: N sequential function
// calls in a single transaction.
func SimpleWorkflow(h cleat.HostCalls, input SimpleInput) (SimpleOutput, error) {
	for i := 0; i < input.Steps; i++ {
		if _, err := h.DurableCall("bench", "noop", `{}`); err != nil {
			return SimpleOutput{}, err
		}
	}
	return SimpleOutput{Done: true}, nil
}
