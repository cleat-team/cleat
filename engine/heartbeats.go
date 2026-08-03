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

	go func() {
		resp, err := s.engine.caller.Call(callCtx, service, operation, requestJSON)
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
			if s.engine.signalStore != nil {
				cancelled, _, pollErr := s.engine.signalStore.PollCancellation(ctx, s.engine.workflowID)
				if pollErr == nil && cancelled {
					cancelCall() // Cancel the in-flight call.
				}
			}

		case res := <-resultCh:
			var callErr string
			if res.err != nil {
				callErr = res.err.Error()
			}

			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCall,
				Service:   service,
				Op:        operation,
				Request:   requestJSON,
				Response:  res.resp,
				Err:       callErr,
			}
			s.recordEvent(rec)

			if res.err != nil {
				written, _ := s.writeResult(ctx, m, responsePtr, res.err.Error(), responseMaxLen)
				return packDurableCallResult(int(written), 1, 1)
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
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			if s.engine != nil && s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		// Detect a pending call intent: the external call was dispatched
		// but the outcome was never persisted.  Return ErrAmbiguous so
		// the caller can check the external service before retrying.
		if rec.Err == pendingSentinel {
			if s.engine != nil && s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx)
			}
			ambiguousErr := fmt.Sprintf(
				"[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s was dispatched but the response was not recorded before a crash. Check the external service before retrying.",
				rec.Step, rec.Service, rec.Op)
			written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}
