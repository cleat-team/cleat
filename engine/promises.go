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

				written, _ := s.writeResult(ctx, m, promiseIDPtr, rec.PromiseID, promiseIDMaxLen)
				return packSimpleResult(0, written)
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

	written, _ := s.writeResult(ctx, m, promiseIDPtr, promiseID, promiseIDMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypePromiseResolved {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				written, _ := s.writeResult(ctx, m, resultPtr, rec.PromiseResult, resultMaxLen)
				return packAwaitPromiseResult(written, false, 0)
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
				// Promise was pending in original execution. Check if resolved now.
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
			written, _ := s.writeResult(ctx, m, resultPtr, result, resultMaxLen)
			return packAwaitPromiseResult(written, false, 0)
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

	// Record await and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitPromise,
		PromiseID: promiseID,
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
	if err := s.engine.promiseStore.ResolvePromise(ctx, s.workflowID, promiseID, value); err != nil {
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
	if err := s.engine.promiseStore.RejectPromise(ctx, s.workflowID, promiseID, errMsg); err != nil {
		s.engine.log().ErrorContext(ctx, "reject_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return packSimpleResult(1, 0)
	}
	return 0
}
