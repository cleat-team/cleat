package engine

import (
	"errors"
	"fmt"
)

// ErrFenceLost is returned by the generation-fenced workflow lifecycle
// methods (CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue,
// ContinueAsNew, FinalizeWorkflowSegment) when the fencing UPDATE affected
// zero rows -- i.e. the (workflow_id, worker_id, generation) tuple the
// caller presented no longer matches the row in the database. This happens
// when a worker stalls long enough to be reaped (ReapStaleInstances resets
// status/assigned_to and bumps generation) and is then reclaimed by another
// worker before the stalled worker's segment finishes and tries to persist
// its result. It is an expected, normal occurrence under reaping -- callers
// should treat it as "someone else now owns this workflow" and return
// cleanly rather than retrying or surfacing it as a failure.
var ErrFenceLost = errors.New("fence lost: workflow reassigned to another worker (generation mismatch)")

// ErrorCode classifies errors for retry decisions.
type ErrorCode int

const (
	ErrUnknown          ErrorCode = iota
	ErrTransient                  // retryable (DB connection, timeout)
	ErrPermanent                  // non-retryable (invalid input, not found)
	ErrCancelled                  // workflow cancelled
	ErrTimeout                    // execution timeout
	ErrAmbiguous                  // call outcome unknown after crash (replay found pending intent)
	ErrRetriesExhausted           // retries exhausted
)

// String returns a human-readable representation of the error code
// suitable for storage in the error_code column.
func (c ErrorCode) String() string {
	switch c {
	case ErrTransient:
		return "transient"
	case ErrPermanent:
		return "permanent"
	case ErrCancelled:
		return "cancelled"
	case ErrTimeout:
		return "timeout"
	case ErrAmbiguous:
		return "ambiguous"
	case ErrRetriesExhausted:
		return "retries_exhausted"
	default:
		return "unknown"
	}
}

// CleatError is a typed error with classification for retry decisions.
type CleatError struct {
	Code       ErrorCode
	Op         string // operation that failed
	WorkflowID string
	Err        error // underlying error
}

func (e *CleatError) Error() string {
	if e.WorkflowID != "" {
		return fmt.Sprintf("%s: workflow=%s: %v", e.Op, e.WorkflowID, e.Err)
	}
	// A CleatError with neither Op nor WorkflowID is a classification-only
	// wrap: it exists to carry a Code that errors.As can find, over an error
	// whose message is already complete. Prefixing it would produce ": <msg>",
	// and adding an Op would produce a third redundant prefix on a message
	// that already carries two (IMPROVEMENT-PLAN 3.23). Pass it through.
	if e.Op == "" {
		return fmt.Sprintf("%v", e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *CleatError) Unwrap() error { return e.Err }

// Retryable returns true if the error is transient and can be retried.
func (e *CleatError) Retryable() bool { return e.Code == ErrTransient }

// NewTransientError creates a retryable error (DB connection, timeout).
func NewTransientError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrTransient, Op: op, WorkflowID: workflowID, Err: err}
}

// NewPermanentError creates a non-retryable error (invalid input, not found).
func NewPermanentError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrPermanent, Op: op, WorkflowID: workflowID, Err: err}
}

// NewTimeoutError creates a timeout error.
func NewTimeoutError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrTimeout, Op: op, WorkflowID: workflowID, Err: err}
}

// NewCancelledError creates a cancellation error.
func NewCancelledError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrCancelled, Op: op, WorkflowID: workflowID, Err: err}
}

// NewAmbiguousError creates an ambiguous-outcome error — the call may have
// succeeded but the response was never persisted. The caller should check
// the external service before retrying.
//
// Passing "" for both op and workflowID produces a classification-only wrap
// that leaves the underlying message untouched; see CleatError.Error. That is
// how execSession.classifyFailure tags a failed execution whose replay hit an
// unresolved pending intent, where the message is already built and the only
// thing missing is the code.
func NewAmbiguousError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrAmbiguous, Op: op, WorkflowID: workflowID, Err: err}
}

// NewRetriesExhaustedError creates an error indicating retries were exhausted.
func NewRetriesExhaustedError(op, workflowID string, err error) *CleatError {
	return &CleatError{Code: ErrRetriesExhausted, Op: op, WorkflowID: workflowID, Err: err}
}
