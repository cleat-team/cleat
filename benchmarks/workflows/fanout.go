package workflows

import (
	"github.com/rcownie/durable/durable"
)

// FanOutInput configures the fan-out workflow.
type FanOutInput struct {
	// Children is the number of child workflows to spawn in parallel.
	Children int `json:"children"`
}

// FanOutOutput is the result of the fan-out workflow.
type FanOutOutput struct {
	// Completed is the number of children that completed successfully.
	Completed int `json:"completed"`
}

// FanOutWorkflow spawns N child workflows and waits for all to complete via
// AwaitAllChildren. This benchmarks the framework's ability to manage
// concurrent workflow executions, including child workflow creation and
// result collection.
//
// Equivalent Temporal workflow: a parent workflow that starts N child
// workflows and collects results. Equivalent DBOS: a parent workflow that
// spawns N child workflows and awaits completion.
func FanOutWorkflow(h durable.HostCalls, input FanOutInput) (FanOutOutput, error) {
	runIDs := make([]string, 0, input.Children)
	for i := 0; i < input.Children; i++ {
		runID, err := h.ChildWorkflow("noop_child", `{}`)
		if err != nil {
			return FanOutOutput{}, err
		}
		runIDs = append(runIDs, runID)
	}

	results, err := h.AwaitAllChildren(runIDs)
	if err != nil {
		return FanOutOutput{}, err
	}
	return FanOutOutput{Completed: len(results)}, nil
}

// NoopChild is a trivial child workflow used by FanOutWorkflow. It performs
// a single durable call and returns immediately.
func NoopChild(h durable.HostCalls, inputJSON string) (string, error) {
	if _, err := h.DurableCall("bench", "noop", `{}`); err != nil {
		return `{"error":"call_failed"}`, err
	}
	return `{"status":"ok"}`, nil
}
