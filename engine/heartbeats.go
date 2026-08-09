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
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}

func (s *execSession) freshCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {

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

	ticker := time.NewTicker(time.Duration(heartbeatIntervalMs) * time.Millisecond)
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
			if cancelledByWorkflow && res.err != nil {
				callErr = cancelledCallError
				nonRetryable = true
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
			}
			s.recordEvent(rec)

			if res.err != nil {
				written, _ := s.writeResult(ctx, m, responsePtr, callErr, responseMaxLen)
				return packDurableCallResult(int(written), recordedFailureCode(nonRetryable), 1)
			}
			written, _ := s.writeResult(ctx, m, responsePtr, res.resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}
	}
}

func (s *execSession) replayCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {

	// Consume any heartbeat events that occurred during the call.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType == EventTypeHeartbeat {
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

		written, _ := s.writeResult(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}
