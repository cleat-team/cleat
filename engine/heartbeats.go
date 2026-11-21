package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/internal/telemetry"
)

// heartbeatStep is the heartbeat call as a dispatcher step (cleat#4152, G3). It is a MULTI-RECORD step: a run of heartbeat events, one per tick while
// the call ran, then the call's outcome. A history that ends INSIDE the run is a worker lost mid-call, and the call is run again with c.Prefix.
var heartbeatStep = stepSpec{
	Name:             "call_with_heartbeat",
	DivergenceDetail: requestDivergenceDetail,
	Effect:           effectEffect,
	Records:          recordsMany,
	Want:             EventTypeCall,
	// The guest decodes result<string, call-failure>: a divergence is answered in the durable-call layout (ABI.md, "Return packing" of cleat_call_heartbeat).
	// AfterSuspend is left nil on purpose: a heartbeat call is refused after a suspension by the dispatcher's own stop refusal, as every durable-call-family
	// spec is, and a hook of its own would be a second answer to the same question.
	Result: layoutDurableCall,
	// The call's service and operation identify it. They are compared on EVERY record of the run, heartbeats included.
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.Service != a.s1 || rec.Op != a.s2 {
			return fmt.Sprintf("call %s.%s, recorded %s.%s", a.s1, a.s2, rec.Service, rec.Op)
		}
		return ""
	},
	Role: func(_ stepArgs, rec *EventRecord) recordRole {
		switch rec.EventType {
		case EventTypeHeartbeat:
			return rolePrefix
		case EventTypeCall:
			return roleFinal
		}
		return roleForeign
	},
	DecodeRun: func(c *stepCtx, run []EventRecord) int64 { return c.s.decodeHeartbeatOutcome(c, &run[len(run)-1]) },
	Fresh:     func(c *stepCtx) int64 { return c.s.freshCallWithHeartbeat(c) },
}

func init() { registerStepSpec(&heartbeatStep) }

func (s *execSession) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	return s.step(ctx, m, &heartbeatStep, stepArgs{s1: service, s2: operation, s3: requestJSON, i1: heartbeatIntervalMs, outPtr: responsePtr, outMaxLen: responseMaxLen})
}

// freshCallWithHeartbeat runs the call: the spec's Fresh. c.Prefix is the heartbeats a lost worker's FIRST incarnation recorded when the history ended
// inside the call, and it decides one thing: the step the idempotency key is derived from (below). The events this incarnation records still take the
// session's own steps in order.
func (s *execSession) freshCallWithHeartbeat(c *stepCtx) int64 {
	ctx, m := c.ctx, c.m
	service, operation, requestJSON := c.args.s1, c.args.s2, c.args.s3
	heartbeatIntervalMs := c.args.i1
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen

	// The refusal for a defer segment, a shutdown and a refused event write is the dispatcher's (step), applied before this runs and on both arrival
	// paths (cleat#4143, #4211), so bit 31 wins over everything below as it did when this function made the check itself. This function starts a
	// goroutine and a ticker whose every tick records an event, so a heartbeat call that was not refused in a defer segment would append history to a
	// workflow that has already terminated for as long as the call ran (IMPROVEMENT-PLAN 3.111).

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
		c.RecordsNothing("a non-positive interval is refused before anything is recorded: the guest chose a number the host cannot tick at (cleat#4105)")
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
	// A call resumed after a lost worker keeps the step its FIRST incarnation started at (the first heartbeat is recorded at the step the call
	// STARTED at: nothing else is recorded while it runs), so every incarnation carries one idempotency key (cleat#4247). Without this the key was derived from the step AFTER the consumed heartbeats, a different key from the first incarnation's, and a
	// service that deduplicates on the key performed the call again. The retry call does the same for the same reason (cleat#1145).
	if len(c.Prefix) > 0 {
		callStep = c.Prefix[0].Step
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
			c.Record(rec)

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
			c.Record(rec)

			if res.err != nil {
				written, _ := s.writeResult(ctx, m, responsePtr, callErr, responseMaxLen)
				return packDurableCallResult(int(written), recordedFailureCode(nonRetryable), 1)
			}
			written, writtenEC := s.writeOut(ctx, m, responsePtr, res.resp, responseMaxLen)
			return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
		}
	}
}

// decodeHeartbeatOutcome answers a replayed heartbeat call from its outcome record, the last of the run the dispatcher read. The heartbeats before it
// are consumed by the dispatcher and carry nothing to decode.
func (s *execSession) decodeHeartbeatOutcome(c *stepCtx, rec *EventRecord) int64 {
	ctx, m := c.ctx, c.m
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen
	// Detect a pending call intent: the external call was dispatched
	// but the outcome was never persisted.  Return ErrAmbiguous so
	// the caller can check the external service before retrying.
	if rec.isPendingIntent() {
		if s.engine != nil && s.engine.Metrics != nil {
			s.engine.Metrics.RecordAmbiguousCall(ctx)
		}
		// See the identical call in durablecalls.go: the structural
		// record is what makes this queryable; the message below is not.
		s.recordAmbiguity(*rec)

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
