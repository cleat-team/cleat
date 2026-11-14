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

// ErrSearchUnavailableUnderEncryption is returned by ListWorkflows when a
// caller asks for InputContains or ErrorContains while
// --encrypt-sensitive-payloads is on.
//
// cleat#2312. Both filters compile to a SQL LIKE against the input/error_msg
// COLUMN, and an encrypted column holds ciphertext, not the plaintext a
// substring search is written against -- so the search would not error, it
// would silently return zero rows (or, once in an astronomically unlikely
// while, a false match on ciphertext that happens to contain the substring).
// A caller reading "no matching workflows" would have no way to tell that
// apart from "I was right, there are none" -- the result LOOKS like an
// answer. Refusing the call outright is the fail-loud alternative: it tells
// the caller the filter cannot do what it says, rather than letting the
// absence of an error stand in for a false "no".
var ErrSearchUnavailableUnderEncryption = errors.New(
	"input/error substring search is not available with --encrypt-sensitive-payloads: " +
		"these columns are ciphertext on disk, so a LIKE search against them cannot find a match")

// ErrQueryStateFilterUnavailableUnderEncryption is returned by ListWorkflows and CountWorkflows when a
// caller filters by published query state while --encrypt-sensitive-payloads is on. cleat#3775.
//
// With the flag on, query_state is not an object but a JSON string literal wrapping base64
// ciphertext, so `query_state @> '{"order_id":"123"}'` is false for every row: the filter would not
// fail, it would answer "no run has that", which is exactly what a caller would read as "there is
// no such run". Refusing it outright is the same choice ErrSearchUnavailableUnderEncryption made.
//
// The stores keep refusing. What changed in part 2 is the HTTP layer: where the store is a
// QueryStateWindowSearcher and --query-state-search-window is not 0, GET /api/workflows does not call
// ListWorkflows for this filter at all and answers from a window search that says what it covered. This
// error is still what a store that cannot search a window, or a worker with the window switched off, returns.
var ErrQueryStateFilterUnavailableUnderEncryption = errors.New(
	"filtering by published state is not available with --encrypt-sensitive-payloads: " +
		"query_state is ciphertext on disk, so the database cannot match a key and value in it")

// MaxQueryStateFilterPairs bounds how many key/value pairs one query-state filter may carry. Each
// pair is a predicate every row is tested against (on SQL Server, a subquery per pair), and a caller
// who needs more than a few is looking for something this filter is not.
const MaxQueryStateFilterPairs = 4

// ErrQueryStateFilterTooManyPairs is returned for a query-state filter over MaxQueryStateFilterPairs.
var ErrQueryStateFilterTooManyPairs = fmt.Errorf(
	"a query-state filter takes at most %d key/value pairs", MaxQueryStateFilterPairs)

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
	ErrResultRejected             // the store refused the workflow result as it was written
	ErrOperator                   // an operator's manual force-fail, not a classification the engine derived (cleat#1977, D5)
)

// namedErrorCodes is every ErrorCode with a String() case below, i.e. every
// code an operator-supplied error_code is allowed to name (cleat#1977, D5).
// This is a second, hand-written list beside the switch in String() --
// ErrorCode has no iota range to range over safely, since String()'s default
// case exists precisely because ErrorCode(99) is a legal, if unclassified,
// value, so there is no way to derive one list from the other the way
// isSettledStatus derives from settledStatusList. TestErrorCode_String pins
// every case in the switch by name; keep this list in step with it by hand
// when either changes.
var namedErrorCodes = []ErrorCode{
	ErrUnknown, ErrTransient, ErrPermanent, ErrCancelled, ErrTimeout,
	ErrAmbiguous, ErrRetriesExhausted, ErrResultRejected, ErrOperator,
}

// IsRecognizedErrorCodeString reports whether s is the String() form of a
// named ErrorCode -- the set an operator-supplied force-fail error_code must
// belong to (cleat#1977, D5). Empty string is not recognized; ForceFail
// substitutes ErrOperator.String() for an empty code before this is ever
// asked.
func IsRecognizedErrorCodeString(s string) bool {
	for _, c := range namedErrorCodes {
		if c.String() == s {
			return true
		}
	}
	return false
}

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
	case ErrResultRejected:
		return "result_rejected_by_store"
	case ErrOperator:
		return "operator"
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
