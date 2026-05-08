package host

import "fmt"

// ErrorCode classifies errors for retry decisions.
type ErrorCode int

const (
	ErrUnknown   ErrorCode = iota
	ErrTransient           // retryable (DB connection, timeout)
	ErrPermanent           // non-retryable (invalid input, not found)
	ErrCancelled           // workflow cancelled
	ErrTimeout             // execution timeout
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
