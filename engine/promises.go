package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero/api"
)

// errNoPromiseStore is what the guest is told when the engine has no promise
// store. It names the option because that is the fix, and the option existing
// while nothing called it is exactly how this survived (IMPROVEMENT-PLAN 3.231).
const errNoPromiseStore = "no promise store configured on this engine (engine.WithPromiseStore was never called); " +
	"a durable promise cannot be created, awaited or settled without one"

func (s *execSession) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeCreatePromise {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}

				written, writtenEC := s.writeOut(ctx, m, promiseIDPtr, rec.PromiseID, promiseIDMaxLen)
				return packSimpleResult(writtenEC, written)
			}
		}
		s.exitReplay()
	}

	// Fresh execution: generate promise ID.
	id, err := uuid.NewRandom()
	var promiseID string
	if err != nil {
		promiseID = fmt.Sprintf("prom-%s-%d", s.workflowID, s.stepCount)
	} else {
		promiseID = id.String()
	}

	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeCreatePromise,
		PromiseName: name,
		PromiseID:   promiseID,
	}
	s.recordEvent(rec)

	// Persist to the promise store, and REPORT A FAILURE rather than logging it.
	//
	// This used to log and continue, returning errCode 0 to the guest. The
	// consequence was not "history and the store disagree" -- it was a hang.
	// The event above already asserts the promise exists, the guest gets an ID
	// and proceeds, and the later AwaitPromise calls GetPromise, finds nothing,
	// falls past both the resolved and rejected branches, and SUSPENDS. It
	// waits for a promise no external caller can ever resolve, because the row
	// they would resolve against was never written. Nothing errors and the log
	// line is the only trace. IMPROVEMENT-PLAN 3.218.
	//
	// The ABI has always had somewhere to put this: cleat_create_promise
	// returns errCode in bits 0-31 (ABI.md 2.34). The failure was not
	// unreportable, it was unreported.
	// A MISSING store is the same failure as a failing one, and until now only
	// the second was reported. 3.218 fixed the error branch below and left the
	// nil branch returning success -- which is the exact defect its own comment
	// names, one line up. The engine offers WithPromiseStore and the worker did
	// not call it (3.231), so on a real deployment this branch was ALWAYS the
	// nil one: every create returned success and every await hung.
	if s.engine.promiseStore == nil {
		s.engine.log().ErrorContext(ctx, "create_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		written, _ := s.writeResult(ctx, m, promiseIDPtr, errNoPromiseStore, promiseIDMaxLen)
		return packSimpleResult(1, written)
	}
	if err := s.engine.promiseStore.CreatePromise(ctx, s.workflowID, name, promiseID); err != nil {
		s.engine.log().ErrorContext(ctx, "create_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		written, _ := s.writeResult(ctx, m, promiseIDPtr, err.Error(), promiseIDMaxLen)
		return packSimpleResult(1, written)
	}

	written, writtenEC := s.writeOut(ctx, m, promiseIDPtr, promiseID, promiseIDMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	// Set when this call is resuming an await already recorded in history,
	// rather than beginning one. The deadline belongs to the ORIGINAL await.
	var (
		resumingAwait  bool
		awaitAnchorMs  int64
		awaitTimeoutMs int64
	)

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypePromiseResolved {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				written, writtenEC := s.writeOut(ctx, m, resultPtr, rec.PromiseResult, resultMaxLen)
				return packAwaitPromiseResult(written, false, uint16(writtenEC))
			}
			if rec.EventType == EventTypePromiseRejected {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				written, _ := s.writeResult(ctx, m, resultPtr, rec.PromiseError, resultMaxLen)
				return packAwaitPromiseResult(written, false, 1)
			}
			if rec.EventType == EventTypeAwaitPromise {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				// Promise was pending in original execution. Check if resolved
				// now -- and carry the ORIGINAL await's anchor and timeout
				// forward, because this wake has to be measured against when
				// the await began, not against itself. See the suspend at the
				// bottom of this function.
				awaitAnchorMs = rec.TimestampMs
				awaitTimeoutMs = rec.TimeoutMs
				resumingAwait = true
				s.exitReplay()
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: check promise store.
	//
	// With no store there is nothing to await and nothing that could ever
	// resolve it, so falling through to the suspend below waits forever. Say so
	// instead. IMPROVEMENT-PLAN 3.231.
	if s.engine.promiseStore == nil {
		s.engine.log().ErrorContext(ctx, "await_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		written, _ := s.writeResult(ctx, m, resultPtr, errNoPromiseStore, resultMaxLen)
		return packAwaitPromiseResult(written, false, 1)
	}
	{
		status, result, errMsg, err := s.engine.promiseStore.GetPromise(ctx, s.workflowID, promiseID)
		if err == nil && status == "resolved" {
			rec := EventRecord{
				Step:          s.stepCount,
				EventType:     EventTypePromiseResolved,
				PromiseID:     promiseID,
				PromiseResult: result,
			}
			s.recordEvent(rec)
			written, writtenEC := s.writeOut(ctx, m, resultPtr, result, resultMaxLen)
			return packAwaitPromiseResult(written, false, uint16(writtenEC))
		}
		if err == nil && status == "rejected" {
			rec := EventRecord{
				Step:         s.stepCount,
				EventType:    EventTypePromiseRejected,
				PromiseID:    promiseID,
				PromiseError: errMsg,
			}
			s.recordEvent(rec)
			written, _ := s.writeResult(ctx, m, resultPtr, errMsg, resultMaxLen)
			return packAwaitPromiseResult(written, false, 1)
		}
	}

	// Still pending. Either begin an await or resume the one already recorded.
	//
	// Resuming is the case that was wrong. This used to record ANOTHER
	// await_promise event and suspend again with Until = now + timeout, so the
	// deadline was recomputed from the current instant on every wake and could
	// never be reached. Measured: a 5000ms timeout still `ready` at generation
	// 31 three minutes later, burning a worker slot per wake. The timeout
	// itself fired correctly -- the workflow woke 5.7s in -- there was simply
	// nothing that reported it, only something that re-armed it. #814.
	//
	// The anchor it needs was already in history: the timestamp of the await
	// event that began the wait. Carrying it makes the deadline a fixed point
	// rather than one that moves with the observer, which is what a durable
	// timeout has to be.
	//
	// AwaitSignals reaches the same outcome a weaker way: on any wake with no
	// signal recorded it reports timedOut, on the reasoning that only the
	// deadline could have woken it ("should not happen", says the comment).
	// That is true today and stops being true the moment anything else wakes a
	// suspended workflow. Comparing against the recorded deadline does not
	// depend on why we woke.
	if resumingAwait {
		deadlineMs := awaitAnchorMs + awaitTimeoutMs
		if awaitTimeoutMs <= 0 {
			// An await recorded before the timeout was persisted, or one with
			// no timeout at all. Nothing to measure against, so keep waiting
			// rather than inventing a deadline -- but do not re-record the
			// await, which is what produced the duplicate events.
			s.suspendErr = &SuspendError{
				Reason: fmt.Sprintf("await_promise(%s)", promiseID),
				Until:  time.Now().Add(time.Duration(timeoutMs) * time.Millisecond),
			}
			return packAwaitPromiseResult(0, true, 0)
		}
		// time.Now(), not nowMs.Load(). The latter is a CACHED clock refreshed
		// on poll cycles (UpdateNowMs), and a cached clock that lags the real
		// one turns this comparison false while the scheduler, which uses real
		// time, sees the deadline as already past. The workflow is then woken
		// immediately, re-suspends against the same past deadline, and spins.
		// Measured with the cached clock: the timeout was reported correctly
		// but only after generation 3130, about 34 wakes a second for 91
		// seconds. A deadline has to be compared against the same clock the
		// scheduler wakes on.
		if time.Now().UnixMilli() >= deadlineMs {
			// Timed out. No event is recorded for this and none is needed:
			// "the deadline has passed" is monotone, so every later replay
			// reaching this point decides the same way. Time only moves
			// forward, which is the property that makes the outcome stable
			// without a durable record of it.
			return packAwaitPromiseResult(0, true, 0)
		}
		// Woke early -- something other than the deadline. Wait out the
		// REMAINDER of the original deadline, and record nothing: the await
		// event is already in history.
		s.suspendErr = &SuspendError{
			Reason: fmt.Sprintf("await_promise(%s)", promiseID),
			Until:  time.UnixMilli(deadlineMs),
		}
		return packAwaitPromiseResult(0, true, 0)
	}

	// First time: record the await and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitPromise,
		PromiseID: promiseID,
		// Persisted so a later wake can measure against the original deadline.
		// await_signals has always recorded its timeout; await_promise did not,
		// which is why the deadline had to be recomputed from scratch.
		TimeoutMs: timeoutMs,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_promise(%s)", promiseID),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packAwaitPromiseResult(0, true, 0)
}

func (s *execSession) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType == EventTypePromiseResolved {
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record and dispatch.
	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: value,
	}
	s.recordEvent(rec)

	// Reported, not logged-and-swallowed. cleat_resolve_promise's adapter
	// already decodes `errCode := uint32(result)` and turns a non-zero into an
	// error, so the ABI has always had somewhere to put this -- the same
	// sentence 3.218 wrote about CreatePromise, still true here afterwards.
	// A resolve that silently did nothing leaves every awaiter suspended.
	if s.engine.promiseStore == nil {
		s.engine.log().ErrorContext(ctx, "resolve_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		return packSimpleResult(1, 0)
	}
	if err := s.engine.promiseStore.ResolvePromise(ctx, promiseID, value); err != nil {
		s.engine.log().ErrorContext(ctx, "resolve_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return packSimpleResult(1, 0)
	}
	return 0
}

func (s *execSession) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType == EventTypePromiseRejected {
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record and dispatch.
	rec := EventRecord{
		Step:         s.stepCount,
		EventType:    EventTypePromiseRejected,
		PromiseID:    promiseID,
		PromiseError: errMsg,
	}
	s.recordEvent(rec)

	// Same as ResolvePromise above, and for the same reason.
	if s.engine.promiseStore == nil {
		s.engine.log().ErrorContext(ctx, "reject_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		return packSimpleResult(1, 0)
	}
	if err := s.engine.promiseStore.RejectPromise(ctx, promiseID, errMsg); err != nil {
		s.engine.log().ErrorContext(ctx, "reject_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return packSimpleResult(1, 0)
	}
	return 0
}
