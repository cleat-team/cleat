package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
)

type ChildResult struct {
	RunID  string `json:"run_id"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
type ChildWorkflowOptions struct {
	Version int // 0 = use default resolution (parent's version)

	// ParentClosePolicy determines what happens to this child workflow when
	// the parent workflow completes or fails.
	// Default: ParentClosePolicyAbandon (current behavior, children continue running).
	ParentClosePolicy ParentClosePolicy

	// Priority controls scheduling order. 0 = highest priority;
	// lower numbers are scheduled first. Children do NOT inherit
	// the parent's priority.
	Priority int
}

func (h *HostCallsImpl) ChildWorkflow(name, inputJSON string) (string, error) {
	if h.childWorkflow == nil {
		return "", errors.New("durable: ChildWorkflow can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.childWorkflow(name, inputJSON)
}

func (h *HostCallsImpl) ChildWorkflowWithOptions(name, inputJSON string, opts ChildWorkflowOptions) (string, error) {
	if h.childWorkflowWithOptions != nil {
		return h.childWorkflowWithOptions(name, inputJSON, opts.Version, string(opts.ParentClosePolicy), opts.Priority)
	}
	// Fall back to plain ChildWorkflow if options handler is not available.
	// ParentClosePolicy defaults to Abandon in this case (current behavior).
	return h.ChildWorkflow(name, inputJSON)
}

func (h *HostCallsImpl) AwaitChild(runID string) (string, error) {
	if h.awaitChild == nil {
		return "", errors.New("durable: AwaitChild can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.awaitChild(runID)
}

func (h *HostCallsImpl) AwaitAllChildren(runIDs []string) ([]ChildResult, error) {
	if h.awaitAllChildren == nil {
		return nil, errors.New("durable: AwaitAllChildren can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.awaitAllChildren(runIDs)
}

func (h *HostCallsImpl) AwaitAnyChild(runIDs []string) (string, string, error) {
	if h.awaitAnyChild == nil {
		return "", "", errors.New("durable: AwaitAnyChild can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.awaitAnyChild(runIDs)
}

func (h *HostCallsImpl) PollChild(runID string) (string, string, error) {
	if h.pollChild == nil {
		return "", "", errors.New("durable: PollChild can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.pollChild(runID)
}

func (h *HostCallsImpl) ChildWorkflowTyped(name string, request interface{}) (string, error) {
	if h.childWorkflowTyped != nil {
		return h.childWorkflowTyped(name, request)
	}
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("durable: marshaling child workflow input for %s: %w", name, err)
	}
	return h.ChildWorkflow(name, string(reqJSON))
}

func (h *HostCallsImpl) AwaitChildTyped(runID string, result interface{}) error {
	if h.awaitChildTyped != nil {
		return h.awaitChildTyped(runID, result)
	}
	resp, err := h.AwaitChild(runID)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling child result for %s: %w", runID, err)
	}
	return nil
}
