package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
	"go.opentelemetry.io/otel/attribute"

	"github.com/cleat-team/cleat/internal/telemetry"
)

// Atomic counters for throughput computation. Sampled by the worker outside of
// the Prometheus registry to avoid prometheus.Collect overhead.
var (
	replayStepCount int64
	freshStepCount  int64
	freshCallCount  int64
)

// ReplayStepCount returns the total replay step count from the atomic counter.
func ReplayStepCount() int64 { return atomic.LoadInt64(&replayStepCount) }

// FreshStepCount returns the total fresh step count from the atomic counter.
func FreshStepCount() int64 { return atomic.LoadInt64(&freshStepCount) }

// FreshCallCount returns the total fresh DurableCall count from the atomic counter.
func FreshCallCount() int64 { return atomic.LoadInt64(&freshCallCount) }

func (s *execSession) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
	}
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	atomic.AddInt64(&freshCallCount, 1)

	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordCall(ctx)
		s.engine.Metrics.RecordFreshStep(ctx, s.defName)
	}

	// Check cancellation before making the call.
	callCtx := ctx
	if s.engine.signalStore != nil {
		cancelled, _, err := s.engine.signalStore.PollCancellation(ctx, s.engine.workflowID)
		if err != nil {
			// Failing open is the right default -- a database blip must not
			// abort a workflow that has not been cancelled -- but it was
			// previously silent, so a persistently failing poll made
			// cancellation quietly stop working with nothing to see.
			s.engine.log().WarnContext(ctx, "cancellation poll failed",
				"workflow_id", s.engine.workflowID, "step", s.stepCount, "error", err)
		}
		if err == nil && cancelled {
			// Not retryable: the workflow was cancelled, so repeating the call
			// is the one thing the caller must not do.
			written, _ := s.writeResult(ctx, m, responsePtr, cancelledCallError, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}
	}

	// Check event cap: if the number of events has reached the limit, auto-trigger
	// ContinueAsNew to start a fresh run with reset event_count. Events are
	// tracked locally in the session (no DB query per call).
	if s.engine.maxEventsPerWorkflow > 0 && s.eventCount >= s.engine.maxEventsPerWorkflow && !s.autoContinueAsNewTriggered {
		s.autoContinueAsNewTriggered = true
		if s.engine.Metrics != nil {
			s.engine.Metrics.RecordContinueAsNew(ctx, "event_cap")
		}
		s.engine.log().InfoContext(ctx, "auto-ContinueAsNew triggered", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "event_count", s.eventCount, "max", s.engine.maxEventsPerWorkflow)
		s.ContinueAsNew(ctx, m, s.originalInput)
		m.CloseWithExitCode(ctx, 0)
		written, _ := s.writeResult(ctx, m, responsePtr, "", responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}
	s.eventCount++

	step := s.stepCount
	callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "call", service, operation)
	defer eventSpan.End()
	callStart := time.Now()

	// An operation declared WriteAheadIntent commits a pending row before the
	// call is dispatched, so a crash mid-call is detectable on replay instead
	// of silently repeating the side effect. It persists the event itself --
	// see freshCallWithIntent for why it must not also flow through
	// recordEvent -- so it returns here rather than falling through.
	if s.engine.callSemantics(service, operation) == WriteAheadIntent {
		resp, err := s.freshCallWithIntent(callCtx, service, operation, requestJSON, step)
		if DebugTiming {
			s.engine.log().InfoContext(ctx, "TIMING: freshCallWithIntent", "workflow_id", s.workflowID, "step", step, "call_ms", time.Since(callStart).Milliseconds())
		}
		if err != nil {
			written, _ := s.writeResult(ctx, m, responsePtr, err.Error(), responseMaxLen)
			return packDurableCallResult(int(written), callFailureCode, 1)
		}
		written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	resp, err := s.callService(callCtx, service, operation, requestJSON, step)
	callElapsed := time.Since(callStart)

	var callErr string
	if err != nil {
		callErr = err.Error()
	}

	rec := EventRecord{
		Step:      step,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
		Err:       callErr,
	}
	s.recordEvent(rec)

	if DebugTiming {
		s.engine.log().InfoContext(ctx, "TIMING: freshCall", "workflow_id", s.workflowID, "step", rec.Step, "call_ms", callElapsed.Milliseconds())
	}

	if err != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, err.Error(), responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {

	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordReplayStep(ctx, s.defName)
	}
	atomic.AddInt64(&replayStepCount, 1)

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeCall {
			if s.engine.Metrics != nil {
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
			if s.engine.Metrics != nil {
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
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx)
			}

			// Ask, before giving up. A resolver that can look the operation up
			// by its idempotency key turns most ambiguities into non-events:
			// the outcome is recorded and replay carries on as though the call
			// had returned normally, which it did -- the crash lost the answer,
			// not the effect. See IMPROVEMENT-PLAN 1.4 phase E.
			if resp, resolved := s.resolveAmbiguity(ctx, rec); resolved {
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordAmbiguousCall(ctx, attribute.String("outcome", "resolved"))
				}
				written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
				return packDurableCallResult(int(written), 0, 0)
			}

			// Record the condition structurally before writing the message.
			// The message is for the workflow author; this is for the
			// operator, who needs to find these with a query rather than a
			// substring search. See IMPROVEMENT-PLAN 3.24.
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
			// The classification comes off the event now rather than being
			// assumed, so a call the caller marked non-retryable replays as
			// non-retryable. Events written before 2.35 carry no such key and
			// read back false, which is the constant this used to hardcode.
			written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), recordedFailureCode(rec.ErrNonRetryable), 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

func (s *execSession) DurableCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Worker-enforced ceiling on retry attempts to prevent runaway retries
	// from misconfigured WASM modules.  Use the engine-configured limit if
	// set (it comes from --max-retries on the command line), otherwise the
	// package-level constant.
	ceiling := MaxRetryAttempts
	if s.engine.maxRetries > 0 && s.engine.maxRetries < ceiling {
		ceiling = s.engine.maxRetries
	}
	if maxAttempts > int64(ceiling) {
		maxAttempts = int64(ceiling)
	}
	if s.isReplay {
		return s.replayCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
	}
	return s.freshCallWithRetry(ctx, m, service, operation, requestJSON,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
		nonRetryableErrorsJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Parse non-retryable error patterns.
	//
	// A parse failure is refused rather than swallowed. This used to drop the
	// error, which left the slice nil, and a nil slice is indistinguishable from
	// "the author declared no non-retryable errors": isDefinitelyNonRetryable
	// finds nothing to match and every failure becomes retryable. So a workflow
	// that said "do not retry INSUFFICIENT_FUNDS" got its call retried
	// maxAttempts times, which for the non-idempotent operations that
	// declaration exists to protect is a duplicate side effect.
	//
	// Note that the interface check in isDefinitelyNonRetryable short-circuits
	// ahead of the pattern list, so this only bites errors classified by message
	// -- which is exactly what this argument is for.
	//
	// nonRetryableErrorsJSON arrives across the ABI from five language SDKs, the
	// layer CLAUDE.md records as the source of four real defects. An SDK sending
	// a bare comma-separated string rather than a JSON array lands here.
	//
	// Failing closed is the safe direction: "I could not read your safety
	// declaration" must not be treated as "you made no safety declaration".
	// badParamDurableCall rather than errBadParam because the guest adapter
	// decodes the packDurableCallResult layout -- see memory.go for what the raw
	// sentinel did to it.
	var nonRetryableErrors []string
	if nonRetryableErrorsJSON != "" {
		if err := json.Unmarshal([]byte(nonRetryableErrorsJSON), &nonRetryableErrors); err != nil {
			s.engine.log().WarnContext(ctx, "unparseable non-retryable error list; refusing the call",
				"service", service, "operation", operation, "error", err)
			return badParamDurableCall
		}
	}

	var lastErr error
	exhausted := true

	// One step for every attempt, so every attempt carries the same idempotency
	// key. That is the intent: a retry of a call that may already have been
	// performed is exactly the case a key exists to collapse.
	retryStep := s.stepCount
	for attempt := int64(1); attempt <= maxAttempts; attempt++ {
		resp, callErr := s.callService(ctx, service, operation, requestJSON, retryStep)

		if callErr == nil {
			// Success — record one event and return.
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCall,
				Service:   service,
				Op:        operation,
				Request:   requestJSON,
				Response:  resp,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}

		lastErr = callErr

		// Check if error is definitively non-retryable.
		if isDefinitelyNonRetryable(callErr, nonRetryableErrors) {
			exhausted = false
			break
		}

		if attempt < maxAttempts {
			// This failure is about to trigger another attempt -- record it as
			// a retry. Nothing else in this loop is visible in Prometheus: the
			// only event history entry freshCallWithRetry writes is the final
			// success or failure, so a call that failed and retried 50 times
			// before succeeding looks identical, from the metrics alone, to one
			// that succeeded on the first try. A retry storm should be visible
			// as a rate, not require reading event history after the fact.
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordCallRetry(ctx,
					attribute.String("service", service),
					attribute.String("operation", operation),
				)
			}

			// Exponential backoff using host time (not DurableSleep).
			backoffMs := initialIntervalMs * int64(math.Pow(float64(backoffCoefficient100x)/100.0, float64(attempt-1)))
			if backoffMs > maxIntervalMs {
				backoffMs = maxIntervalMs
			}
			if backoffMs < 1 {
				backoffMs = 1 // minimum backoff to prevent a tight retry loop
			}
			select {
			case <-ctx.Done():
				// errCode 0 here reported a *successful* call with an empty
				// response: the guest's generated adapter branches on
				// `errCode != 0`, so a retry loop abandoned mid-backoff looked
				// to the workflow like the service had answered with "".
				written, _ := s.writeResult(ctx, m, responsePtr, ctx.Err().Error(), responseMaxLen)
				return packDurableCallResult(int(written), callErrorUnknown, 1)
			case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			}
		}
	}

	// All retries exhausted or non-retryable error — record failure event.
	errMsg := lastErr.Error()
	if exhausted {
		errMsg = "retries exhausted: " + errMsg
	}
	// The loop broke early precisely because this error is not worth retrying.
	// Reporting it as retryable told the workflow to do the one thing the
	// engine had just decided against -- and for a non-idempotent operation a
	// caller marks non-retryable, that is a duplicate side effect.
	//
	// Recording the bit is what makes fixing it legal: replay reads this event
	// back, and if it could not recover the classification the same step would
	// be non-retryable on the first run and retryable on the replay of it.
	nonRetryable := !exhausted
	rec := EventRecord{
		Step:            s.stepCount,
		EventType:       EventTypeCall,
		Service:         service,
		Op:              operation,
		Request:         requestJSON,
		Err:             errMsg,
		ErrNonRetryable: nonRetryable,
	}
	s.recordEvent(rec)

	// Retries exhausted stays callFailureCode: the error was retryable, the
	// attempts simply ran out, and calling again later may well work.
	written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
	return packDurableCallResult(int(written), recordedFailureCode(nonRetryable), 1)
}

func (s *execSession) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	// Sleep is local: it records no event. So what tells us whether a sleep has
	// already been served is not history but time.
	//
	// The anchor is the timestamp of the last event the workflow recorded -- a
	// real moment, written when that step ran. Every sleep since then pushes a
	// virtual deadline further past it. If that deadline is already behind real
	// time, the wait has happened, whether it was spent suspended, sitting in a
	// queue, or with the worker down. If it is still ahead, this is a genuinely
	// new wait and the workflow suspends for the remainder.
	//
	// This replaces the replayJustEnded flag, which asked a question sleep
	// cannot answer for itself: "did some *other* durable call just cross the
	// replay frontier?" In a faithful replay none does -- every earlier
	// operation was recorded and replay-matches -- so the sleep was always what
	// reached the end of history, and it re-suspended forever. It also replaces
	// the narrower frontier check that fixed only the single-sleep case.
	// IMPROVEMENT-PLAN 3.67.
	//
	// max() rather than assignment, and this is the subtle part: Now() reads
	// history[stepCount-1] while stepCount is within history, and sleeps do not
	// advance stepCount. Two sleeps in a row would otherwise read the *same*
	// anchor, compute the same deadline, and the second would complete a wait
	// it never performed -- the same "completing on someone else's evidence"
	// failure as the bug this replaces, just one step further along.
	anchor := s.nowMs
	if n := s.Now(ctx); n > anchor {
		anchor = n
	}
	if anchor <= 0 {
		// No anchor: a fresh workflow that has recorded nothing yet, and a
		// nowMs seed that was never set. The package-level seed is refreshed by
		// the worker's dispatch loop but by nothing in the CLI and embedded
		// paths, where it stays zero -- and an anchor at the epoch puts every
		// deadline decades in the past, so every sleep would complete instantly
		// and no workflow run under `cleat run` or cleatctl would ever wait.
		anchor = s.engine.realNowMs()
	}
	s.nowMs = anchor + durationMs

	if s.nowMs <= s.engine.realNowMs() {
		return packSleepResult(sleepStatusCompleted, 0)
	}

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("cleat_sleep(%dms)", durationMs),
		Until:  time.UnixMilli(s.nowMs),
	}

	return packSleepResult(sleepStatusSuspend, durationMs)
}

func (s *execSession) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeDefer {
				// Recompute the ID before advanceReplayStep, which bumps
				// stepCount. The fallback is what the fresh path below would
				// have minted at this same step, so a history written before
				// DeferID was recorded still reconstructs the ID the guest was
				// originally handed.
				deferID := rec.DeferID
				if deferID == "" {
					deferID = fmt.Sprintf("defer-%d", s.stepCount)
				}
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}

				// Re-register, do not just re-answer. This branch is the only
				// one a defer registered in an earlier segment ever reaches
				// again -- every later segment replays past its registration --
				// so skipping the map write dropped the defer permanently. The
				// fresh path below does both halves; replay must too.
				desc := rec.DeferDescription
				if desc == "" {
					desc = description
				}
				s.mu.Lock()
				s.deferrals[deferID] = desc
				s.mu.Unlock()

				written, _ := s.writeResult(ctx, m, deferIDPtr, deferID, deferIDMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
	}

	deferID := fmt.Sprintf("defer-%d", s.stepCount)

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeDefer,
		DeferDescription: description,
		DeferID:          deferID,
	}
	s.recordEvent(rec)

	s.mu.Lock()
	s.deferrals[deferID] = description
	s.mu.Unlock()

	written, _ := s.writeResult(ctx, m, deferIDPtr, deferID, deferIDMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	// Non-durable: no event recorded, no replay matching.
	// Log output goes via the worker's stdout/stderr capture.
	return 0
}

func (s *execSession) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	if s.isReplay {
		// On replay, skip (fire-and-forget is recorded but not re-executed).
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
		}
		return 0
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeDurableSend,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
	}
	s.recordEvent(rec)

	// Capture the step before the goroutine starts. Reading s.stepCount from
	// inside it would race with the session advancing, and would key the call to
	// whatever step happened to be current when the goroutine was scheduled.
	sendStep := rec.Step

	// Execute the fire-and-forget call through the caller.
	// Wrap in a timeout context to bound goroutine lifetime in case
	// the external Call blocks indefinitely.
	if s.engine.caller != nil {
		go func() {
			if ctx.Err() != nil {
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			_, _ = s.callService(callCtx, service, operation, requestJSON, sendStep)
		}()
	}
	return 0
}

func (s *execSession) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
		}
		return 0
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeDurableScheduleInvoke,
		Service:    service,
		Op:         operation,
		Request:    requestJSON,
		DurationMs: delayMs,
	}
	s.recordEvent(rec)

	// As in DurableSend: capture before the goroutine, not inside it.
	scheduleStep := rec.Step

	// Schedule the call. For now, run in a goroutine after the delay.
	// Wrap the call in a timeout context to bound goroutine lifetime
	// in case the external Call blocks indefinitely.
	if s.engine.caller != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				_, _ = s.callService(callCtx, service, operation, requestJSON, scheduleStep)
			}
		}()
	}
	return 0
}
