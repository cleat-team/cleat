package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/internal/telemetry"
)

func (s *execSession) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
	}
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen, -1)
}

// freshCallWithHeartbeat runs the call. resumeStep is -1 for a call that starts here, and otherwise the step a lost worker's FIRST incarnation
// started the call at, recovered from the heartbeats it recorded (replayCallWithHeartbeat). It decides one thing: the step the idempotency key is
// derived from. The events this incarnation records still take the session's own steps in order.
func (s *execSession) freshCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32, resumeStep int) int64 {
	// A defer segment's body is refused new work (IMPROVEMENT-PLAN 3.84; the heartbeat call was the eighth fresh path and the last in the
	// durable-call family, added in 3.111). The refusal lives HERE and not in DurableCallWithHeartbeat: this function is reached from the
	// non-replay entry AND from replayCallWithHeartbeat at the replay frontier, and a defer segment over a run that has history starts
	// in replay, so its body's first new call arrives by the second way. A refusal only in the entry was skipped (cleat#4143, measured
	// on a real worker: a terminate's segment made the body's next call and never ran the registered cleanup). SetScope's refusal
	// (cleat#4091) and AcquireLock's (cleat#4140) are now the dispatcher's, for both ways in (cleat#4211).
	//
	// It is first, before the interval check below and before anything is recorded: ABI.md requires bit 31 to win over any field this
	// layout carries, and a refusal records no event.
	//
	// This one is worse than an ordinary unguarded call rather than equal to it. This function starts a goroutine and a ticker, and
	// every tick writes an EventTypeHeartbeat through recordEvent -- so an unstopped heartbeat call in a defer segment does not merely
	// perform the side effect once, it appends history to a workflow that has already terminated, for as long as the call runs.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	if res, ok := s.refuseIfSuspended("DurableCallWithHeartbeat", callSuspendSentinel); ok {
		return res
	}

	// A NON-POSITIVE INTERVAL IS REFUSED BEFORE ANYTHING ELSE, AND THE REFUSAL RECORDS NOTHING. cleat#4105.
	//
	// time.NewTicker panics on an interval of zero or less, and the guest chooses this number: `heartbeat-interval-ms` is a u64 on the
	// WIT function, so 0 is one value away and anything from 2^63 up arrives here negative. Nothing between the guest and the ticker
	// checked it, and the component path has no recover, so the panic took down whatever was executing the workflow. Measured through
	// the engine, 2026-11-19: `panic: non-positive interval for NewTicker`.
	//
	// Refused rather than clamped to a minimum, because there is no honest minimum to pick: the interval is how often the claim is
	// renewed, and the guest that sent 0 has a bug the call cannot repair. No event is recorded, so a replay never meets a refusal it
	// did not record, and no service call is made. A value too LARGE is a different case and is not refused: it is clamped below, and
	// a heartbeat that never ticks inside a hundred years is a legitimate request.
	if heartbeatIntervalMs <= 0 {
		written, _ := s.writeResult(ctx, m, responsePtr,
			fmt.Sprintf("cleat: heartbeat interval must be at least 1 ms, got %d (the host cannot tick at a non-positive interval)", heartbeatIntervalMs),
			responseMaxLen)
		return packDurableCallResult(int(written), callErrorInvalidRequest, 1)
	}

	step := s.stepCount
	ctx, eventSpan := telemetry.EventSpan(ctx, step, "call_heartbeat", service, operation)
	defer eventSpan.End()

	type callResult struct {
		resp string
		err  error
	}
	resultCh := make(chan callResult, 1)

	// Create a cancellable context for the call so we can cancel it if
	// the workflow is cancelled during a long-running heartbeat call.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()

	// Captured before the goroutine: reading s.stepCount from inside it races
	// with the session advancing.
	callStep := s.stepCount
	// A call resumed after a lost worker keeps the step its FIRST incarnation started at, so every incarnation carries one idempotency key
	// (cleat#4247). Without this the key was derived from the step AFTER the consumed heartbeats, a different key from the first incarnation's, and a
	// service that deduplicates on the key performed the call again. The retry call does the same for the same reason (cleat#1145).
	if resumeStep >= 0 {
		callStep = resumeStep
	}

	// Set when the heartbeat loop cancels the call because the workflow was
	// cancelled, so the result branch can tell that apart from the call failing
	// on its own. Without it the two are indistinguishable: cancelCall() makes
	// the caller return a context error like any other, and the call would be
	// reported as an ordinary retryable failure.
	//
	// Written and read on this goroutine only -- the ticker case and the result
	// case are arms of the same select -- so no synchronisation is needed.
	cancelledByWorkflow := false

	go func() {
		resp, err := s.callService(callCtx, service, operation, requestJSON, callStep)
		resultCh <- callResult{resp: resp, err: err}
	}()

	ticker := time.NewTicker(guestMsDuration(heartbeatIntervalMs))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeHeartbeat,
				Service:   service,
				Op:        operation,
			}
			s.recordEvent(rec)

			// Check for cancellation on each heartbeat tick.
			//
			// A failing poll is deliberately not treated as a cancellation: a
			// database blip must not abort a call that is running normally, and
			// the poll repeats on the next tick, so failing open costs at most
			// one interval. See TestDurableCallWithHeartbeat_PollErrorDoesNotCancel.
			if s.engine.signalStore != nil {
				cancelled, _, pollErr := s.engine.signalStore.PollCancellation(ctx, s.engine.workflowID)
				if pollErr != nil {
					s.engine.log().WarnContext(ctx, "heartbeat cancellation poll failed",
						"workflow_id", s.engine.workflowID, "step", callStep, "error", pollErr)
				}
				if pollErr == nil && cancelled {
					cancelledByWorkflow = true
					cancelCall() // Cancel the in-flight call.
				}
			}

		case res := <-resultCh:
			var callErr string
			nonRetryable := false
			if res.err != nil {
				callErr = res.err.Error()
			}

			// A call the heartbeat loop cancelled is reported as a cancellation,
			// not as whatever error the cancelled context produced. freshCall
			// already does this before dispatch; this is the same outcome for a
			// call cancelled after it started.
			//
			// Both halves matter. The guest-visible code has to be
			// callErrorUnknown, which callerrors.go documents as non-retryable
			// with a cancelled workflow as its first example -- reporting
			// callFailureCode instead tells a guest branching on Retryable() to
			// re-issue the call it was just cancelled out of. And the *recorded*
			// event has to carry the same classification, because replay reads
			// retryability off the event: an event holding the raw context error
			// with ErrNonRetryable unset replays as an ordinary retryable
			// failure, so the same step would be non-retryable on the first run
			// and retryable on the replay of it. recordedFailureCode exists to
			// stop exactly that.
			// errClass is the third half of the same story, added by
			// IMPROVEMENT-PLAN 2.35: the bit above says what the engine did,
			// this says how the failure was classified. A cancellation is the
			// one case here the engine classifies itself rather than reading
			// off the caller, and it is worth recording as a class -- it is
			// what lets an operator query for calls cut short by a
			// cancellation instead of grepping for the message.
			errClass := recordedErrorClass(res.err)
			if cancelledByWorkflow && res.err != nil {
				callErr = cancelledCallError
				nonRetryable = true
				errClass = ErrCancelled.String()
			}

			rec := EventRecord{
				Step:            s.stepCount,
				EventType:       EventTypeCall,
				Service:         service,
				Op:              operation,
				Request:         requestJSON,
				Response:        res.resp,
				Err:             callErr,
				ErrNonRetryable: nonRetryable,
				ErrCode:         errClass,
			}
			s.recordEvent(rec)

			if res.err != nil {
				written, _ := s.writeResult(ctx, m, responsePtr, callErr, responseMaxLen)
				return packDurableCallResult(int(written), recordedFailureCode(nonRetryable), 1)
			}
			written, writtenEC := s.writeOut(ctx, m, responsePtr, res.resp, responseMaxLen)
			return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
		}
	}
}

func (s *execSession) replayCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {

	// Consume any heartbeat events that occurred during the call. The first one was recorded at the step the call STARTED at (nothing else is
	// recorded while it runs), which is the step its idempotency key was derived from: remembered in case the history ends inside the call and the
	// call has to be run again (cleat#4247).
	firstBeatStep := -1
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType == EventTypeHeartbeat {
			if firstBeatStep < 0 {
				firstBeatStep = rec.Step
			}
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			continue
		}
		break
	}

	// Now find the matching call event.
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeCall {
			if s.engine != nil && s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			// Not retryable: a divergence is a bug in the workflow code, and
			// running the same call again produces the same divergence.
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}

		if rec.Service != service || rec.Op != operation {
			if s.engine != nil && s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			// Not retryable: a divergence is a bug in the workflow code, and
			// running the same call again produces the same divergence.
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}

		// Detect a pending call intent: the external call was dispatched
		// but the outcome was never persisted.  Return ErrAmbiguous so
		// the caller can check the external service before retrying.
		if rec.isPendingIntent() {
			if s.engine != nil && s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx)
			}
			// See the identical call in durablecalls.go: the structural
			// record is what makes this queryable; the message below is not.
			s.recordAmbiguity(rec)

			ambiguousErr := fmt.Sprintf(
				"[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s was dispatched but the response was not recorded before a crash. Check the external service before retrying.",
				rec.Step, rec.Service, rec.Op)
			// Not retryable, and this is the case the old blanket "timeout"
			// got most wrong: the call may well have succeeded. Telling the
			// guest to retry is telling it to risk a duplicate side effect.
			written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}

		if rec.Err != "" {
			// The classification comes off the event, matching how replayCall
			// (durablecalls.go) handles the same concern for the plain
			// DurableCall path. freshCallWithHeartbeat has persisted
			// ErrNonRetryable since the cancelledByWorkflow branch was added
			// (see the note there); this used to hardcode callFailureCode with
			// a comment claiming the class "was never persisted", which that
			// change made false. Both paths must derive the code from the
			// event the same way, or the same step is retryable on the first
			// run and non-retryable on the replay of it.
			written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), recordedFailureCode(rec.ErrNonRetryable), 1)
		}

		written, writtenEC := s.writeOut(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen, firstBeatStep)
}
