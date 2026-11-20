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
	return s.step(ctx, m, &createPromiseStep, stepArgs{s1: name, outPtr: promiseIDPtr, outMaxLen: promiseIDMaxLen})
}

// createPromiseStep is CreatePromise as a step (cleat#4152). The refusal below is the one the dispatcher applies to every non-Pure step:
// a fresh create_promise is new work, it appends an event that finishDeferPhase writes into the terminal transaction and writes a
// promise row at once. In a defer segment that is the workflow BODY running past its recorded history, so it is refused; a defer body
// still gets through (inDeferDrain). Measured against a real store, cleat#4079 audit row create_promise.
var createPromiseStep = stepSpec{
	Name:    "create_promise",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeCreatePromise,
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, rec.PromiseID, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, m, name := c.s, c.ctx, c.m, c.args.s1
		promiseIDPtr, promiseIDMaxLen := c.args.outPtr, c.args.outMaxLen

		// Fresh execution: generate promise ID.
		id, err := uuid.NewRandom()
		var promiseID string
		if err != nil {
			promiseID = fmt.Sprintf("prom-%s-%d", s.workflowID, s.stepCount)
		} else {
			promiseID = id.String()
		}

		c.Record(EventRecord{
			Step:        s.stepCount,
			EventType:   EventTypeCreatePromise,
			PromiseName: name,
			PromiseID:   promiseID,
		})

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
	},
}

func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	s.setAwaitRejected(false)
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
				s.setAwaitRejected(true)
				return packAwaitPromiseResult(written, false, 1)
			}
			if rec.EventType == EventTypeAwaitPromise {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				// A recorded NO-WAIT await replays as the timeout it was, from the record alone (cleat#4144). The fresh path below
				// records an await with a timeout of 0 or less and returns timed out. Consulting the store here returned RESOLVED to a
				// replay that ran after the promise settled, where the original run had been told it timed out, so a poll loop took a
				// different branch on its first resume; and it did so with history still unread. The guest's own argument must also be
				// non-positive, so a recorded 0 that meant "unknown" (written before the timeout was persisted) and is resumed with a
				// positive argument still takes the resuming path below.
				if rec.TimeoutMs <= 0 && timeoutMs <= 0 {
					return packAwaitPromiseResult(0, true, 0)
				}
				// Promise was pending in original execution. Check if resolved
				// now -- and carry the ORIGINAL await's anchor and timeout
				// forward, because this wake has to be measured against when
				// the await began, not against itself. See the suspend at the
				// bottom of this function.
				awaitAnchorMs = rec.TimestampMs
				awaitTimeoutMs = rec.TimeoutMs

				// HISTORY CONTINUES PAST THIS RECORD: the await was already answered, so answer it from history and STAY IN
				// REPLAY (cleat#4177). A pending await can only be left in two ways, and both leave a trace in what follows:
				//
				//   - it RESOLVED or was REJECTED: the outcome is recorded at the NEXT step, not over the pending row (the fresh path
				//     records at s.stepCount, which advanceReplayStep has just moved past this record). So the record right after this
				//     one is this promise's promise_resolved or promise_rejected.
				//   - it TIMED OUT: nothing is recorded for a timeout ("the deadline has passed" is monotone, below), so the next record
				//     is simply the guest's next call. Any other record means the original run took the timeout.
				//
				// Leaving replay here instead, as this branch always did, ran every durable call recorded after the await a second time
				// on every later replay, and answered `resolved` to a replay that ran after the promise had settled where the original
				// had been told it timed out. Flush's immutability guard kept the duplicates out of the stored history, so the history
				// read as correct while the effects ran twice.
				//
				// WHICH outcome records are the await's own: only one WITHOUT the settlement discriminator. resolve_promise and
				// reject_promise record the same event types with the same PromiseID, and only Service="promise" with Op="resolve"
				// or "reject" tells them apart (cleat#4079, ResolvePromise). A workflow that awaits p, times out, and then resolves p
				// ITSELF with a default records [await_promise, promise_resolved(Op=resolve)] and a call after it: matching on the
				// event type alone read that settlement as the await's answer, so a later replay returned `resolved` where the
				// original run was told it timed out, and the call ran again (measured by cleat-review on cleat#4178).
				//
				// DIRECTION for histories written before cleat#4079, which have no Op on either: such an outcome is read as the
				// await's own, which is what this branch did before it looked ahead at all and what an outcome recorded by the
				// await itself has always looked like. The reading is wrong only for a pre-#4079 workflow that timed out and then
				// resolved the same promise itself as its very next step, and a record cannot tell the two apart. The await's own
				// outcome always carries PromiseID, so an empty one is not the await's (nothing writes one).
				//
				// KNOWN AMBIGUITY, deliberately resolved toward resolved: [await_promise, promise_resolved(p)] with no Op is also what
				// a timed-out await followed IMMEDIATELY by a second await of the same already-settled promise records. They are only
				// both possible when the promise settles in the instant between the timeout and the guest's next call. An await
				// followed by anything else, which is every shape a retry loop makes, is unambiguous.
				if s.stepCount < len(s.history) {
					next := s.history[s.stepCount]
					sameID := next.PromiseID == promiseID && next.Service == "" && next.Op == ""
					switch {
					case next.EventType == EventTypePromiseResolved && sameID:
						if !s.advanceReplayStep(ctx, &next) {
							return 0
						}
						written, writtenEC := s.writeOut(ctx, m, resultPtr, next.PromiseResult, resultMaxLen)
						return packAwaitPromiseResult(written, false, uint16(writtenEC))
					case next.EventType == EventTypePromiseRejected && sameID:
						if !s.advanceReplayStep(ctx, &next) {
							return 0
						}
						written, _ := s.writeResult(ctx, m, resultPtr, next.PromiseError, resultMaxLen)
						s.setAwaitRejected(true)
						return packAwaitPromiseResult(written, false, 1)
					default:
						return packAwaitPromiseResult(0, true, 0)
					}
				}
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
			s.setAwaitRejected(true)
			return packAwaitPromiseResult(written, false, 1)
		}
	}

	// A timeout of 0 (or less) has no deadline to expire, so it must not suspend: cleat#4144, the cleat#1331 loop that await_signals
	// guards against in signaller.go. Placed AFTER the settled checks above, so an already-settled promise is still reported and a
	// 0 ms await degrades to a poll rather than to nothing.
	//
	// The suspensions below set Until = now + 0, a deadline of NOW. The run is immediately re-claimable, wakes, replays to the recorded
	// await, takes the resuming branch (whose awaitTimeoutMs <= 0 arm used to keep waiting with the same deadline of now) and suspends
	// again, forever, each cycle a legitimate claim that changes nothing.
	//
	// Timed out rather than an error, matching await_signals and a durable sleep, where 0 ms means "do not wait". It RECORDS the await,
	// with the timeout it was given, because a replay must return the same answer without asking the store: recording nothing let a
	// replay that ran after the promise settled return RESOLVED where the original run was told it timed out (measured by
	// cleat-review), and left a later event at this step for the replay to trip over. The replay branch above returns the timeout from
	// this record alone. A poll loop therefore records one event per iteration, which counts toward the event cap.
	//
	// Not for a resume that carries a POSITIVE recorded timeout: that deadline belongs to the original await and wins, as it always did.
	// (A recorded timeout of 0 or less with a non-positive argument never arrives here: it returned from the replay branch.)
	if timeoutMs <= 0 && !(resumingAwait && awaitTimeoutMs > 0) {
		// A poll is an event like any other, so it counts toward the event cap and stops at it (cleat#4153). Without this a poll loop
		// kept recording into a quota the flush refuses, retrying each refused write for the whole flush window.
		if s.refuseAtEventCap(ctx, m) {
			return s.suspendingAwaitResult()
		}
		s.recordEvent(EventRecord{
			Step:      s.stepCount,
			EventType: EventTypeAwaitPromise,
			PromiseID: promiseID,
			TimeoutMs: timeoutMs,
		})
		return packAwaitPromiseResult(0, true, 0)
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
		deadlineMs := awaitAnchorMs + clampGuestMs(awaitTimeoutMs) // clamp: i64::MAX wrapped this negative (cleat#4105)
		if awaitTimeoutMs <= 0 {
			// An await recorded before the timeout was persisted, or one with
			// no timeout at all. Nothing to measure against, so keep waiting
			// rather than inventing a deadline -- but do not re-record the
			// await, which is what produced the duplicate events.
			s.suspendErr = &SuspendError{
				Reason: fmt.Sprintf("await_promise(%s)", promiseID),
				Until:  time.Now().Add(guestMsDuration(timeoutMs)),
			}
			return s.suspendingAwaitResult()
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
			//
			// This is the one return in the function that IS a timeout, and
			// it stays a timed-out word. The other three set suspendErr and
			// return s.suspendingAwaitResult(): a suspending await must not
			// hand the guest a word it reads as a timeout (cleat#3383).
			return packAwaitPromiseResult(0, true, 0)
		}
		// Woke early -- something other than the deadline. Wait out the
		// REMAINDER of the original deadline, and record nothing: the await
		// event is already in history.
		s.suspendErr = &SuspendError{
			Reason: fmt.Sprintf("await_promise(%s)", promiseID),
			Until:  time.UnixMilli(deadlineMs),
		}
		return s.suspendingAwaitResult()
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
		Until:  time.UnixMilli(s.nowMs).Add(guestMsDuration(timeoutMs)),
	}

	return s.suspendingAwaitResult()
}

// The discriminator values ResolvePromise and RejectPromise stamp on the events they record (cleat#4079). They are exported
// constants, not literals, because a stop point is chosen by reading them back (isStoppableEvent) and two spellings of one
// word would make the stop silently skip every effect.
const (
	PromiseEventService = "promise"
	PromiseOpResolve    = "resolve"
	PromiseOpReject     = "reject"
)

func (s *execSession) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return s.step(ctx, m, &resolvePromiseStep, stepArgs{s1: promiseID, s2: value})
}

// resolvePromiseStep is ResolvePromise as a step (cleat#4152). Replay matches by event TYPE alone, as it always did: a promise_resolved
// that carries no Service/Op (what AwaitPromise records for a promise that was already settled) is accepted here, which a characterization
// row keeps visible (resolve_promise/replay_by_type_alone). The Service/Op discriminator is the stop point's business, not replay's (cleat#4079).
//
// The refusal the dispatcher applies: a defer segment's body must not settle a promise, that is an effect another run may be waiting on, and
// a segment exists to run cleanup, not to make new calls (cleat#4079, the same refusal DeleteCron got in cleat#4083). It is decided before
// anything is recorded, so a refused call leaves no event.
var resolvePromiseStep = stepSpec{
	Name:    "resolve_promise",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypePromiseResolved,
	// Consumed and answered with success, whatever the original run was told by the store (see the characterization row
	// resolve_promise/store_error_then_replay: nothing in the event says the store write failed).
	Decode: func(*stepCtx, *EventRecord) int64 { return 0 },
	Fresh: func(c *stepCtx) int64 {
		s, ctx, promiseID, value := c.s, c.ctx, c.args.s1, c.args.s2

		// Fresh execution: record and dispatch.
		c.Record(EventRecord{
			Step:      s.stepCount,
			EventType: EventTypePromiseResolved,
			// Service and Op are the discriminator that tells this event from the one AwaitPromise records for a promise that was
			// already resolved: the two have the same event type and the same PromiseID and PromiseResult, and only this one is an
			// effect. A stop point for a defer segment may be placed at this one and never at that one (stoppableFirstEvents).
			// Histories written before cleat#4079 lack it, which is the safe direction: such an event is not a stop point.
			Service:       PromiseEventService,
			Op:            PromiseOpResolve,
			PromiseID:     promiseID,
			PromiseResult: value,
		})

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
	},
}

func (s *execSession) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return s.step(ctx, m, &rejectPromiseStep, stepArgs{s1: promiseID, s2: errMsg})
}

// rejectPromiseStep is RejectPromise as a step (cleat#4152). Refused in a defer segment's body, for the reason resolvePromiseStep gives;
// replay matches by event type alone, as it always did.
var rejectPromiseStep = stepSpec{
	Name:    "reject_promise",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypePromiseRejected,
	Decode:  func(*stepCtx, *EventRecord) int64 { return 0 },
	Fresh: func(c *stepCtx) int64 {
		s, ctx, promiseID, errMsg := c.s, c.ctx, c.args.s1, c.args.s2

		// Fresh execution: record and dispatch. Service and Op: see resolvePromiseStep.
		c.Record(EventRecord{
			Step:         s.stepCount,
			EventType:    EventTypePromiseRejected,
			Service:      PromiseEventService,
			Op:           PromiseOpReject,
			PromiseID:    promiseID,
			PromiseError: errMsg,
		})

		// Same as resolvePromiseStep above, and for the same reason.
		if s.engine.promiseStore == nil {
			s.engine.log().ErrorContext(ctx, "reject_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
			return packSimpleResult(1, 0)
		}
		if err := s.engine.promiseStore.RejectPromise(ctx, promiseID, errMsg); err != nil {
			s.engine.log().ErrorContext(ctx, "reject_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return packSimpleResult(1, 0)
		}
		return 0
	},
}

// setAwaitRejected records whether the AwaitPromise call in progress ended in a
// rejection. The packed result word cannot say so -- a rejection and a host
// error both carry errCode 1 -- and the component path's WIT type has a case for
// each (cleat#3425). Read back through lastAwaitPromiseRejected.
func (s *execSession) setAwaitRejected(rejected bool) {
	s.mu.Lock()
	s.awaitRejected = rejected
	s.mu.Unlock()
}

// lastAwaitPromiseRejected implements awaitRejectionReporter.
func (s *execSession) lastAwaitPromiseRejected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awaitRejected
}
