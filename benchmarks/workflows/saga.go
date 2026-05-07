package workflows

import (
	"fmt"

	"github.com/rcownie/durable/durable"
)

// SagaInput configures the saga compensation workflow.
type SagaInput struct {
	// Steps is the number of saga steps to execute.
	Steps int `json:"steps"`
}

// SagaOutput is the result of the saga workflow.
type SagaOutput struct {
	Done bool `json:"done"`
}

// SagaWorkflow demonstrates the saga compensation pattern with a configurable
// number of steps. Each step has a forward action (a durable API call) and a
// compensation action (another durable API call). All steps succeed, so no
// compensation is triggered. This benchmarks the overhead of the Saga
// scaffolding: step registration, the Run loop, and LogKV calls between
// steps.
//
// Equivalent Temporal: a workflow using a Saga helper with N steps.
// Equivalent DBOS: a workflow using a transaction array with compensation.
func SagaWorkflow(h durable.HostCalls, input SagaInput) (SagaOutput, error) {
	s := durable.NewSaga()
	for i := 0; i < input.Steps; i++ {
		idx := i // capture for closure
		s.AddStep(fmt.Sprintf("step_%d", idx),
			// Forward action
			func(h durable.HostCalls) (string, error) {
				return h.DurableCall("bench", "forward", `{}`)
			},
			// Compensation action
			func(h durable.HostCalls) error {
				_, err := h.DurableCall("bench", "compensate", `{}`)
				return err
			},
		)
	}
	if err := s.Run(h); err != nil {
		return SagaOutput{}, err
	}
	return SagaOutput{Done: true}, nil
}

// SagaWithCompensationInput configures the saga-with-failure benchmark.
type SagaWithCompensationInput struct {
	// Steps is the total number of saga steps (including the failing one).
	Steps int `json:"steps"`
	// FailAtStep is the zero-based index of the step that should fail.
	// All steps before it will be compensated.
	FailAtStep int `json:"fail_at_step"`
}

// SagaWithCompensationOutput is the result of the saga-with-failure workflow.
type SagaWithCompensationOutput struct {
	Compensated int  `json:"compensated"`
	Failed      bool `json:"failed"`
}

// SagaWithCompensationWorkflow runs a saga where one step fails, triggering
// compensation of all previously completed steps. This benchmarks the cost
// of the compensation loop: reverse-order iteration, LogKV calls, and
// compensate function execution.
func SagaWithCompensationWorkflow(h durable.HostCalls, input SagaWithCompensationInput) (SagaWithCompensationOutput, error) {
	s := durable.NewSaga()
	for i := 0; i < input.Steps; i++ {
		idx := i // capture for closure
		s.AddStep(fmt.Sprintf("step_%d", idx),
			func(h durable.HostCalls) (string, error) {
				if idx == input.FailAtStep {
					return "", durable.NewTerminalError(
						fmt.Errorf("simulated failure at step %d", idx),
					)
				}
				return h.DurableCall("bench", "forward", `{}`)
			},
			func(h durable.HostCalls) error {
				_, err := h.DurableCall("bench", "compensate", `{}`)
				return err
			},
		)
	}

	err := s.Run(h)
	if err == nil {
		return SagaWithCompensationOutput{Compensated: 0, Failed: false}, nil
	}

	// Count compensated steps. The failing step itself doesn't get compensated;
	// only previously completed steps do.
	return SagaWithCompensationOutput{
		Compensated: input.FailAtStep,
		Failed:      true,
	}, nil
}
