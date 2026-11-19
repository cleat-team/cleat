package engine

import "context"

// stoppableFirstEvents are the event types that BEGIN a guest host call the engine can refuse with a stop, and so the
// events a replay-after-kill can stop at (cleat#4079).
//
// Only the FIRST event of a call is listed. A call that takes two events (await_signals, then signal_received) or
// several (call_attempt_failed, then call) must not be cut between them: the guest's replay of that call expects all of
// them, so a stop placed at the second would leave the session mid-call.
//
// Sleep is absent because it records no event (see DurableSleep).
//
// WHICH events are listed is not a judgement call: TestEveryHostStopSiteIsClassifiedForTheStoppableSet derives it. An event
// is a stop point only if BOTH hold: a host site refuses the call that records it, and the component world gives a guest a
// way to RECEIVE that refusal (a WIT function with a typed outcome, stopSurfaces[site].wit). The second is what keeps
// delete_cron out: the host refuses it in a segment (cleat#4083), but its WIT function returns nothing, so a component guest
// would never see the stop and would carry on past the refused call, back into the loop the kill interrupted.
//
// A stop point at an event the guest cannot be stopped at is the unsafe error. Leaving an event out is the safe one: the stop
// lands at an earlier event, and what trails it is recorded and skipped rather than replayed.
//
// Absent on purpose, each with its reason in that test: set_query_state, create_promise and set_scope (no refusing site
// today); delete_cron, update_completed (no typed outcome in the component world; resolve_promise and reject_promise ARE
// stop points but only by Op, see stoppableByOp); signal_received and update_received (the
// same event types record DELIVERY, which no call of the guest refuses); await_promise, which only reads, so a cleanup has no
// effect of it to see. await_child, await_all_children and continue_as_new were listed here before that test existed and have
// no refusing site, so they are not.
var stoppableFirstEvents = map[EventType]bool{
	EventTypeCall:                  true,
	EventTypeAwaitSignals:          true,
	EventTypeChildWorkflow:         true,
	EventTypePluginCall:            true,
	EventTypeRunDetached:           true,
	EventTypeSideEffect:            true,
	EventTypeDurableSend:           true,
	EventTypeDefer:                 true,
	EventTypeFetch:                 true,
	EventTypeAcquireLock:           true,
	EventTypeScheduleCron:          true,
	EventTypeDurableScheduleInvoke: true,
}

// stoppableByOp lists the event types that are a stop point only when the event's Op says it was the EFFECT and not a read that
// shares the type: promise_resolved and promise_rejected are recorded both by resolve_promise and reject_promise (effects) and by
// await_promise (a read of an already-settled promise), with the same fields otherwise. A stop at the read would let a promise
// that is already settled return normally and the guest run on inside the segment. Events recorded before the discriminator
// existed have no Op and are not stop points, which is the safe direction.
var stoppableByOp = map[EventType]map[string]bool{
	EventTypePromiseResolved: {PromiseOpResolve: true},
	EventTypePromiseRejected: {PromiseOpReject: true},
}

// isStoppableEvent reports whether rec is the first event of a call a defer segment can stop at.
//
// The Service and Op it reads are NOT in the event checksum: eventRecordToPayload hashes a promise event's promise_id and its result or
// error only (the Service/Op formula in computeEventChecksum is the fallback for a payload that fails to encode, not the normal path).
// That is acceptable here and it is worth saying why, because it is not true of every discriminator. Replay never reads Op, so editing
// it cannot change what a replayed guest sees, only where a killed run's segment stops, and the worst outcome is the safe one: an
// earlier stop, skipping an effect. Changing it needs write access to the event store, which can rewrite any row anyway. A
// discriminator whose value a replay RETURNS to the guest would be a different case and would need to be in the checksum.
func isStoppableEvent(rec EventRecord) bool {
	if stoppableFirstEvents[rec.EventType] {
		return true
	}
	return rec.Service == PromiseEventService && stoppableByOp[rec.EventType][rec.Op]
}

// lastStoppableIndex returns the index in history of the first event of the last guest call the engine can stop at, or -1
// when there is none.
//
// Events written by a defer drain (InDeferPhase) or by a defer segment (InDeferSegment) are not the body's and are never a
// stop point. A call that retried is
// reported at its first attempt, so the stop lands before the whole call and not between its attempts.
//
// Why the LAST stoppable event. The guest was killed somewhere after it. Everything after it that the history records is
// effect-free or unguarded, so replaying only as far as it, and stopping there, performs every effect the original run
// performed. Between that event and the point where the guest was killed, nothing outside the guest could tell this
// replay from a kill that landed during that call. cleat-review's design review on cleat#4079.
func lastStoppableIndex(history []EventRecord) int {
	for i := len(history) - 1; i >= 0; i-- {
		rec := history[i]
		if rec.InDeferPhase || rec.InDeferSegment || !isStoppableEvent(rec) {
			continue
		}
		for i > 0 && history[i-1].EventType == EventTypeCallAttemptFailed {
			i--
		}
		return i
	}
	return -1
}

// fastForwardToStop leaves replay at the stop point: every remaining event, the stop event itself and any that trail it,
// is skipped without being replayed, and the session is no longer replaying. The guest's next host call is the stop
// event's call; it now takes its FRESH path, where stopBeforeNewWork refuses it with a stop and records nothing.
//
// The checksum chain and the virtual clock advance over each skipped event exactly as advanceReplayStep does for a
// consumed one. Skipping them without that would chain the segment's own first event from the wrong predecessor, which
// verification reports as a checksum mismatch, and the segment's events would then be appended after history rows they
// do not chain from.
//
// The skipped events stay in the history, and the segment's events are recorded after them: stepCount is len(history).
func (s *execSession) fastForwardToStop() {
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++
		if rec.TimestampMs > 0 {
			s.nowMs = rec.TimestampMs
		}
		s.lastChecksum = s.chainAnchor.next(rec, s.lastChecksum)
	}
	s.exitReplay()
	s.stopIdx = -1
}

// armStopAtLastStoppable decides, once, where this session stops. It is a no-op unless the engine was asked to stop at
// the last stoppable event, is running a defer segment over a non-empty history, and the guest is a COMPONENT: a core module
// drains its defers in place after a kill, and a core-module segment stops at its frontier as it always has.
func (s *execSession) armStopAtLastStoppable(_ context.Context, isComponent bool) {
	s.stopIdx = -1
	if !isComponent || s.engine == nil || !s.engine.stopAtLastStoppable || !s.engine.deferPhase || !s.isReplay {
		return
	}
	s.stopArmed = true
	s.stopIdx = lastStoppableIndex(s.history)
	if s.stopIdx == 0 {
		// Nothing precedes the stop event, so no consumed event will trigger the fast-forward.
		s.fastForwardToStop()
	}
}
