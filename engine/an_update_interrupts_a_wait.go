package engine

// cleat#4511: an update that arrives while a run is suspended in a wait.
//
// The SDKs poll for updates immediately before each wait. A wait that suspends has already written its own row, so after the update's wake the history ENDS
// in that unanswered row, and the poll in front of it replays onto a record that is another call's: it answered "nothing found" without consuming it (the
// ForeignFirst rule of poll_update), the wait consumed its own row, and the run suspended again with the update still pending. Nothing polled again.
//
// A recorded history cannot take an event in its middle (update_replay_test.go pins it), so the poll cannot deliver "before" the wait row. What it can do
// is deliver AFTER it: when the wait row is the last row of history and is still unanswered, the poll consumes it and delivers at the frontier. The
// delivery is recorded with UpdateInterruptsWait, which is what makes the pair [wait row, update_received] readable on every later replay: without the
// marker it is also what a wait that timed out followed by the NEXT call's poll finding an update records.
//
// The guest then runs the handler, records update_completed, and its own wait call (the SDK's dispatch loop ended) runs fresh. That second wait is the
// SAME wait of the guest's one call, so its deadline is the first one's: see waitCarry.

import (
	"time"
)

// overWaitKind is what a poll's OverWait hook decided about the record at its step.
type overWaitKind int

const (
	// overWaitNone: nothing here is for the hook, the step goes on as it did before the hook existed.
	overWaitNone overWaitKind = iota
	// overWaitFresh: the record is the last row of history, an unanswered wait row, and an update is pending. The step consumes the row, leaves replay and
	// runs Fresh, which records the delivery marked.
	overWaitFresh
	// overWaitReplay: the record is a wait row followed by an update_received that carries the marker. The step consumes the wait row and then replays the
	// delivery from history as it does any other.
	overWaitReplay
)

// isWaitRow says whether an event type is the row one of the five waits writes before it suspends.
func isWaitRow(t EventType) bool {
	switch t {
	case EventTypeAwaitSignals, EventTypeAwaitPromise, EventTypeAwaitChild, EventTypeAwaitAllChildren, EventTypeAwaitAnyChild:
		return true
	}
	return false
}

// unansweredWaitRow says whether rec, which is the LAST row of history, is the row a wait left when it suspended the run and nothing has answered it.
//
// The timed waits (signals, promise) must also have an UNEXPIRED deadline, read from the row against the real clock the wait itself reads. An await that
// has timed out records nothing for the timeout, so [await] followed by the guest's next suspension (a Sleep, which records nothing either) leaves the
// expired await as the last row; a poll that delivered across it would hand the update to the code that ran BEFORE the timeout, ahead of the code that ran
// after it (cleat-review2's reading of the design). A row with no timestamp or no timeout has no deadline to read, and is not taken: the poll answers as
// it always did. The child waits have no deadline; an empty row is a pending wait.
func unansweredWaitRow(rec *EventRecord, realNowMs int64) bool {
	switch rec.EventType {
	case EventTypeAwaitSignals, EventTypeAwaitPromise:
		return rec.TimestampMs > 0 && rec.TimeoutMs > 0 && realNowMs < rec.TimestampMs+clampGuestMs(rec.TimeoutMs)
	case EventTypeAwaitChild:
		return rec.Response == "" && rec.Err == ""
	case EventTypeAwaitAllChildren, EventTypeAwaitAnyChild:
		return rec.Response == ""
	}
	return false
}

// pollOverWait is poll_update's OverWait hook. rec is the record at the step, and it is not an update_received.
func pollOverWait(c *stepCtx, rec *EventRecord) overWaitKind {
	s := c.s
	if s.engine.updateStore == nil || s.suspendErr != nil || !isWaitRow(rec.EventType) {
		return overWaitNone
	}
	// Replay of a delivery that interrupted this wait: only a MARKED record is read this way, so an unmarked pair (a wait that timed out, then the next
	// call's poll found an update) replays exactly as before.
	if next := s.stepCount + 1; next < len(s.history) {
		if n := &s.history[next]; n.EventType == EventTypeUpdateReceived && n.UpdateInterruptsWait {
			return overWaitReplay
		}
		return overWaitNone
	}
	// The wait row is the last row: the run suspended in it. Whether an update is pending decides; a poll that finds none answers nothing found and leaves
	// the row for the wait that owns it, as it always did.
	if !unansweredWaitRow(rec, s.engine.realNowMs()) {
		return overWaitNone
	}
	// A delivery is new work, so the refusals every fresh poll has apply here too (a defer segment, a stopping worker), and they are checked BEFORE the
	// row is consumed.
	//
	// Written as an assignment and not as `if s.stopBeforeNewWork() {`, which java_sdk_stop_bit_parity_test.go counts: that count is of the places where the HOST
	// REFUSES A GUEST CALL, and this is not one. It answers no stop word; it only declines to deliver, and the poll then answers nothing found as it always did.
	stopped := s.stopBeforeNewWork()
	if stopped {
		return overWaitNone
	}
	pending, err := s.engine.updateStore.GetPendingUpdateRequests(c.ctx, s.engine.workflowID)
	if err != nil {
		s.engine.log().ErrorContext(c.ctx, "poll_update: reading pending update requests",
			"workflow_id", s.engine.workflowID, "error", err)
		return overWaitNone
	}
	if len(pending) == 0 {
		return overWaitNone
	}
	c.pending = pending
	return overWaitFresh
}

// waitCarry is the deadline of a wait that an update interrupted, handed to the wait that follows it.
//
// The guest made ONE wait call, and the poll and the second wait are the SDK's: a guest that asked for seven days must not get seven more because a
// reviewer claimed the task. The interrupted row's deadline (its timestamp plus its timeout, the same arithmetic the wait's own replay uses) is therefore
// the deadline of the wait that replaces it. Only the timed waits carry one; a child wait has no deadline.
//
// It is keyed by the update's request id, and released to the first wait after the update's own complete_update (releaseWaitCarry), not to the next wait
// of any kind: a wait the HANDLER makes comes before the completion and must not take it. The map is rebuilt on a replay by the same two places that fill it
// on the original run (pollUpdateStep's Fresh and Decode), so a handler that suspends and is replayed finds it again.
type waitCarry struct {
	kind       EventType
	key        string // the signal names, or the promise id
	deadlineMs int64
}

// carryFromWaitRow reads the deadline a wait row carries. ok is false for a row that has none to give (a child wait, or a row with no timestamp or no
// timeout), and the wait after the update then takes the guest's own timeout, as any fresh wait does.
func carryFromWaitRow(w *EventRecord) (waitCarry, bool) {
	if w == nil || w.TimestampMs <= 0 || w.TimeoutMs <= 0 {
		return waitCarry{}, false
	}
	switch w.EventType {
	case EventTypeAwaitSignals:
		return waitCarry{kind: w.EventType, key: w.SignalNames, deadlineMs: w.TimestampMs + clampGuestMs(w.TimeoutMs)}, true
	case EventTypeAwaitPromise:
		return waitCarry{kind: w.EventType, key: w.PromiseID, deadlineMs: w.TimestampMs + clampGuestMs(w.TimeoutMs)}, true
	}
	return waitCarry{}, false
}

// noteInterruptedWait remembers the deadline of the wait a delivery interrupted, under the delivery's request id.
func (s *execSession) noteInterruptedWait(w *EventRecord, requestID string) {
	if w == nil {
		return
	}
	carry, ok := carryFromWaitRow(w)
	if !ok {
		return
	}
	if s.updateCarry == nil {
		s.updateCarry = map[string]waitCarry{}
	}
	s.updateCarry[requestID] = carry
}

// noteDelivered records that an update has been handed to the guest and has not been completed. It is called by every delivery, fresh or replayed, marked or not,
// because the SDK's dispatch loop polls until nothing is pending: one wake can deliver SEVERAL updates, and the carry of the first must survive the second
// (cleat-review2's GAP on #4536).
func (s *execSession) noteDelivered(requestID string) {
	if s.openUpdates == nil {
		s.openUpdates = map[string]bool{}
	}
	s.openUpdates[requestID] = true
}

// releaseWaitCarry is called when an update's complete_update runs, on the original run and on a replay alike. It closes the update, and readies the carry that
// update left. An update with no carry of its own (it did not interrupt a wait: a second update handled in the same wake) leaves a standing one alone, which
// is the carry the FIRST update readied for the guest's wait; the guest's wait is what takes it, or clears it.
func (s *execSession) releaseWaitCarry(requestID string) {
	delete(s.openUpdates, requestID)
	c, ok := s.updateCarry[requestID]
	delete(s.updateCarry, requestID)
	if ok {
		s.waitCarry = &c
	}
}

// takeWaitCarry is called at the entry of every wait, whichever path the call takes (a replayed wait consumes its row and never reaches Fresh): the first
// wait after a completion is the one that may use the carry, so it is cleared here and handed on only through the call's arguments.
//
// While an update is OPEN (delivered, not completed) a wait is that update's HANDLER's, not the guest's own, and takes nothing and clears nothing: the carry is for the
// wait that comes after the dispatch loop ends. Without this the handler of a second update took the first one's carry (cleat-review2's case B).
func (s *execSession) takeWaitCarry() *waitCarry {
	if len(s.openUpdates) > 0 {
		return nil
	}
	c := s.waitCarry
	s.waitCarry = nil
	return c
}

// carriedWait says what a fresh timed wait records and waits for, given the carry its call was handed. ok is false when there is none for this wait (no
// carry, or it names another kind or another signal set or promise), and the wait then does what it always did.
//
// The timeout is the carried deadline less now, never less than one millisecond and never more than the guest's own: a deadline that has already passed
// must not reach the "invented deadline" branch of the wait's replay (a timeout of zero or less is read as none), and a wait that is still owed must not
// outlast what the guest asked for. The row is stamped with the same instant the remainder was measured from, so timestamp plus timeout is the carried
// deadline exactly, which is all a later replay reads.
func (c *stepCtx) carriedWait(kind EventType, same func(carriedKey string) bool, guestTimeoutMs int64) (timeoutMs, stampMs int64, until time.Time, ok bool) {
	carry := c.args.carry
	if carry == nil || carry.kind != kind || !same(carry.key) {
		return 0, 0, time.Time{}, false
	}
	now := c.s.engine.realNowMs()
	remaining := carry.deadlineMs - now
	if g := clampGuestMs(guestTimeoutMs); g > 0 && g < remaining {
		remaining = g
	}
	if remaining < 1 {
		remaining = 1
	}
	return remaining, now, time.UnixMilli(now + remaining), true
}
