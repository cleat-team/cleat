package engine

// Call error classification.
//
// These mirror cleat.CallErrorCode in the guest SDK, which is what a workflow
// author's `switch e.Code` and `e.Retryable()` read. They are redeclared here
// rather than imported because engine does not depend on the cleat package;
// engine/callerrors_test.go asserts the two agree, so they cannot drift
// silently.
//
// Only the three the engine actually packs are declared. Timeout, NotFound and
// PermissionDenied exist in the guest enum but nothing here can select them --
// declaring them would be three constants kept alive by their own drift test,
// which is the shape this repo has been removing rather than adding.
//
// Retryable, per cleat.CallError.Retryable(): Timeout and Unavailable. Every
// other value -- including Unknown -- is non-retryable.
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
// This is the narrow half of IMPROVEMENT-PLAN 2.35. The full error class still
// has nowhere to come from: no ServiceCaller in the repo returns anything but a
// bare fmt.Errorf, so a richer taxonomy would be values nothing populates --
// which is how engine/flush.go accumulated 350 lines of durability code that
// had never run (docs/durable-call-intent-design.md).
func recordedFailureCode(nonRetryable bool) byte {
	if nonRetryable {
		return callErrorUnknown
	}
	return callFailureCode
}
