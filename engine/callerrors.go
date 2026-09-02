package engine

import "errors"

// Call error classification.
//
// These mirror the guest SDK's CallErrorCode enum, which is what a workflow
// author's `switch e.Code` and `e.Retryable()` read. They are redeclared here
// rather than imported because engine must not depend on the cleat/ SDK module:
// cleat/ depends on engine, so an import in this direction is a module cycle,
// and the pair of `replace` directives that used to resolve it is what made
// `go install github.com/cleat-team/cleat/cmd/cleat@vX` refuse to run.
// GuestCallErrorCodes below is the mirror, and the cleat/ module's
// callerror_contract_test.go checks it against the real enum.
//
// Only the three the engine actually packs are declared as constants. Timeout,
// NotFound and PermissionDenied exist in the guest enum but nothing here can
// select them -- declaring them as engine constants would be three values kept
// alive by their own drift test, which is the shape this repo has been removing
// rather than adding. They appear in GuestCallErrorCodes, because that table
// describes the *guest's* enum rather than the engine's usage of it.
//
// Retryable, per the guest's CallError.Retryable(): Timeout and Unavailable.
// Every other value -- including Unknown -- is non-retryable.
const (
	// callErrorUnknown is the classification for a failure the engine itself
	// produced -- a cancelled workflow, a replay divergence, an ambiguous
	// outcome. Non-retryable, which is the point: none of them is fixed by
	// calling again, and the ambiguous case may already have succeeded.
	callErrorUnknown byte = 0
	// callErrorUnavailable is the classification for a call that failed for a
	// reason the engine cannot identify. Retryable, as the old hardcoded
	// CallErrorTimeout was, so workflows branching on Retryable() are
	// unaffected -- what changes is that it stops claiming the call *timed
	// out* when the engine has no idea what happened.
	callErrorUnavailable byte = 2
	// callErrorInvalidRequest is the classification for a request the host
	// refused to interpret. See badParamDurableCall in memory.go.
	callErrorInvalidRequest byte = 4
)

// callFailureCode is the code reported for a call that the *service* failed
// and that is worth trying again, as opposed to a failure the engine itself
// produced.
const callFailureCode = callErrorUnavailable

// GuestCallErrorCode describes one member of the guest SDK's CallErrorCode
// enum as the engine understands it.
type GuestCallErrorCode struct {
	// Name is the constant's name with its CallError prefix stripped:
	// "Unknown" is Go's cleat.CallErrorUnknown, Rust's CallError::Unknown,
	// and the corresponding member in the Python, AssemblyScript and Java
	// SDKs.
	Name string
	// Code is the byte the engine packs into the classification field of a
	// durable-call result. This is wire ABI -- every guest SDK decodes it, so
	// a member can be added but an existing value can never be changed.
	Code byte
	// Retryable is what the guest reports for this code: CallError.Retryable()
	// in the Go SDK, and its equivalent elsewhere.
	Retryable bool
}

// guestCallErrorCodes is the engine's copy of the guest SDK's CallErrorCode
// enum, in value order.
//
// Exported through GuestCallErrorCodes because the engine cannot import the
// SDK to check itself (see the comment on the constants above), so the check
// has to run from the other side of the boundary. The cleat/ module's
// callerror_contract_test.go asserts this table against cleat.CallError*
// exhaustively -- every member, its value, and its retryability, in both
// directions. Nothing else stops the two copies drifting, and a drifted code
// is worse than no code: the guest's `switch e.Code` falls through to default,
// so the structured classification silently degrades to "something failed".
//
// Retryability is part of the table rather than derived here because it is a
// guest-side decision the engine only mirrors. It was previously not checked
// against the SDK at all.
var guestCallErrorCodes = []GuestCallErrorCode{
	{Name: "Unknown", Code: 0, Retryable: false},
	{Name: "Timeout", Code: 1, Retryable: true},
	{Name: "Unavailable", Code: 2, Retryable: true},
	{Name: "NotFound", Code: 3, Retryable: false},
	{Name: "InvalidRequest", Code: 4, Retryable: false},
	{Name: "PermissionDenied", Code: 5, Retryable: false},
}

// GuestCallErrorCodes returns the engine's copy of the guest SDK's
// CallErrorCode enum, in value order.
//
// A copy, so a caller cannot edit the table the contract test reads.
func GuestCallErrorCodes() []GuestCallErrorCode {
	out := make([]GuestCallErrorCode, len(guestCallErrorCodes))
	copy(out, guestCallErrorCodes)
	return out
}

// cancelledCallError is the message a durable call reports when the workflow
// was cancelled while the call was in flight.
//
// A constant rather than a literal because two paths produce it -- freshCall
// checks before dispatch, freshCallWithHeartbeat checks on every heartbeat tick
// -- and because replay compares against what was recorded. A cancelled call is
// non-retryable: repeating it is the one thing a cancelled workflow must not do.
const cancelledCallError = "workflow cancelled"

// recordedFailureCode maps a recorded call failure to the code the guest sees.
//
// Both the fresh path and the replay path must go through this function. A
// recorded failure is replayed from the event history, so if the two derived
// the classification differently the same step would be retryable on the first
// run and non-retryable on the replay of it -- a determinism bug in the engine,
// introduced in the name of better error reporting. Routing both through one
// function is what keeps that from being a convention two call sites have to
// remember; TestFreshAndReplayAgreeOnNonRetryableFailure asserts it end to end,
// replaying the event the fresh run actually recorded.
//
// callErrorUnknown, not callErrorInvalidRequest, for the non-retryable case.
// Both are non-retryable, which is the bit that matters, but the engine does
// not know *why* the caller declined the retry -- ServiceCaller returns a bare
// error and the only machine-readable signal is RetryableError's single bool.
// Reporting InvalidRequest would tell the workflow author their request was
// malformed, which is a claim nothing here supports.
//
// This is the narrow half of IMPROVEMENT-PLAN 2.35, and it stays narrow on
// purpose even though the wide half now exists.
//
// The paragraph here used to read "no ServiceCaller in the repo returns
// anything but a bare fmt.Errorf, so a richer taxonomy would be values nothing
// populates". That was true when written and stopped being true in the same
// section's next update: dbServiceCaller (cmd/cleat-worker/setup.go), the only
// ServiceCaller that runs in production, returns NewPermanentError /
// NewTransientError throughout. The class is recorded now --
// EventRecord.ErrCode, written by recordedErrorClass.
//
// What has not changed is that this function must not read it. The recorded
// class and the recorded bit can legitimately disagree, because
// DurableCallWithRetry's nonRetryableErrors list comes from the guest's own
// retry policy across the ABI: a workflow author can declare a substring
// non-retryable for an error whose CleatError says ErrTransient. The bit is
// what the engine acted on, so the bit is what the guest must be told, and
// deriving the code from the class instead would change the retry behaviour of
// workflows already in flight.
func recordedFailureCode(nonRetryable bool) byte {
	if nonRetryable {
		return callErrorUnknown
	}
	return callFailureCode
}

// recordedErrorClass returns the engine's classification of a failed call, as
// the string EventRecord.ErrCode stores, or "" when the error carries none.
//
// "" rather than "unknown" for an unclassified error, which is the whole
// reason this does not just call ErrorCode.String(): ErrUnknown is the iota
// zero value, so an error that no ServiceCaller classified and one classified
// *as* unknown would otherwise be written identically. Empty means "nobody
// said", and it is also what every event written before IMPROVEMENT-PLAN 2.35's
// second half reads back as.
//
// errors.As, not a type assertion: a CleatError is routinely wrapped by the
// time it reaches here -- DurableCallWithRetry's loop adds context -- and the
// same traversal is what isDefinitelyNonRetryable already uses to find
// RetryableError, so the two agree about which error in a chain is speaking.
func recordedErrorClass(err error) string {
	if err == nil {
		return ""
	}
	var ce *CleatError
	if !errors.As(err, &ce) {
		return ""
	}
	if ce.Code == ErrUnknown {
		return ""
	}
	return ce.Code.String()
}
