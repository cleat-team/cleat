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

// callFailureCode is the code reported for a call that the *service* failed,
// as opposed to a failure the engine itself produced.
//
// It is a constant rather than a classification of the error, and that is a
// deliberate constraint rather than laziness. A recorded call failure comes
// back from the event history as a bare string (EventRecord.Err), so replay
// cannot recover any class the fresh path might have derived. Deriving one on
// the fresh path and not on replay would make the same step retryable on the
// first run and non-retryable on the replay of it -- a determinism bug in the
// engine, introduced in the name of better error reporting.
//
// Classifying at the ServiceCaller boundary therefore requires persisting the
// code alongside the event first. That is IMPROVEMENT-PLAN 2.35, and no
// mechanism for it is added here: an interface nothing can call yet is how
// engine/flush.go accumulated 350 lines of durability code that had never run
// (see docs/durable-call-intent-design.md).
const callFailureCode = callErrorUnavailable
