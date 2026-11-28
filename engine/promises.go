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

// settlementIdentity is the identity check of resolve_promise and reject_promise: the record must name the promise the workflow is settling,
// and when it carries the cleat#4079 Service/Op discriminator it must be the settlement of this kind. A record WITHOUT the discriminator is
// accepted, and there are three sources of such a record (cleat-review3, read in the code): a history written before cleat#4079; the record
// await_promise writes for a promise that was already settled; and the typed-event conversion in events.go, whose PromiseResolvedEvent and
// PromiseRejectedEvent keep the promise id and the result or error and drop Service and Op. Pinned by the replay_by_type_alone row.
//
// What this cannot catch, and no rule can: a resolve_promise(P) that meets an await's record for the same P. The two records are the same
// bytes in the history format, so that is a limit of the format and not of the check.
func settlementIdentity(promiseID string, rec *EventRecord, op string) string {
	if rec.PromiseID != promiseID {
		return fmt.Sprintf("promise %q, recorded %q", promiseID, rec.PromiseID)
	}
	if rec.Service != "" && (rec.Service != PromiseEventService || rec.Op != op) {
		return fmt.Sprintf("a %s, recorded as %s/%s", op, rec.Service, rec.Op)
	}
	return ""
}

// createPromiseStep is CreatePromise as a step (cleat#4152). The refusal below is the one the dispatcher applies to every non-Pure step:
// a fresh create_promise is new work, it appends an event that finishDeferPhase writes into the terminal transaction and writes a
// promise row at once. In a defer segment that is the workflow BODY running past its recorded history, so it is refused; a defer body
// still gets through (inDeferDrain). Measured against a real store, cleat#4079 audit row create_promise.
var createPromiseStep = stepSpec{
	Name:               "create_promise",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypeCreatePromise,
	// A promise is identified by its NAME: the id is generated, and replay hands back the recorded one.
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.PromiseName != a.s1 {
			return fmt.Sprintf("promise name %q, recorded %q", a.s1, rec.PromiseName)
		}
		return ""
	},
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		// A promise the store refused to create replays as the refusal, with the message the original run was handed (cleat#4335). An event with no
		// Err is a success, which is every event recorded before this field was set on the call.
		if rec.Err != "" {
			written, _ := c.s.writeResult(c.ctx, c.m, c.args.outPtr, rec.Err, c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
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

		// THE STORE IS WRITTEN FIRST AND THE EVENT RECORDS HOW IT WENT (cleat#4335, which closed cleat#4203). The order used to be the reverse: the event
		// was recorded, then the store written, and a write that failed was told to the guest and recorded nowhere, so a replay answered success where the
		// original run had been told an error. An event is persisted when it is recorded, so it cannot be amended afterwards; the outcome has to be known
		// first. The failure is carried in Err, the field run_detached's refused start uses (cleat#4114), and Decode above answers from it.
		//
		// What the order changes is the crash window, and it is the better one. A crash between the two used to leave an event whose row was never written:
		// the replay answered success with an id nobody had created, and every await of it suspended for ever (the hang IMPROVEMENT-PLAN 3.218 describes).
		// It now leaves a row whose event was never recorded: the replay runs the call again, generates a NEW id, and the first row is an orphan that
		// no guest was ever given. See also cleat#4335 for the per-store reading of whether repeating the write is safe.
		//
		// A MISSING store is the same failure as a failing one (3.231: the worker did not call WithPromiseStore, so this branch was ALWAYS the nil one and
		// every create returned success while every await hung), and it is recorded the same way.
		var storeErr string
		if s.engine.promiseStore == nil {
			s.engine.log().ErrorContext(ctx, "create_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
			storeErr = errNoPromiseStore
		} else if err := s.engine.promiseStore.CreatePromise(ctx, s.workflowID, name, promiseID); err != nil {
			// REPORTED, not logged and continued: the guest used to be told success, got an id nothing would ever settle, and hung on it (3.218).
			// cleat_create_promise returns errCode in bits 0-31 (ABI.md 2.34), so the ABI has always had somewhere to put this.
			s.engine.log().ErrorContext(ctx, "create_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			storeErr = err.Error()
		}

		c.Record(EventRecord{
			Step:        s.stepCount,
			EventType:   EventTypeCreatePromise,
			PromiseName: name,
			PromiseID:   promiseID,
			Err:         storeErr,
		})
		if storeErr != "" {
			written, _ := s.writeResult(ctx, m, promiseIDPtr, storeErr, promiseIDMaxLen)
			return packSimpleResult(1, written)
		}

		written, writtenEC := s.writeOut(ctx, m, promiseIDPtr, promiseID, promiseIDMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

func init() { registerStepSpec(&createPromiseStep) }

// AwaitPromise is await_promise as a step (cleat#4152, W4). The wrapper does two things the step cannot: it clears the rejection flag the component
// path reads (lastAwaitPromiseRejected), and it answers the one case where an engine with NO promise store meets a history it cannot replay (below).
//
// The no-store exception. With no promise store configured an await records NOTHING (it returns errNoPromiseStore), so the record at its step
// belongs to the NEXT call, and on replay the await meets a foreign first record. Before the move that was answered with the same error, consuming
// nothing and staying in replay; the dispatcher would report it as a divergence, which it is not (nothing is wrong with the history, the engine just
// cannot ask). So when there is no store, the session is replaying, and the first record is not one of the call's own types, the answer is the
// error and nothing is consumed. A history that DOES hold the call's own records still replays without a store (a CLI replay of a real history).
// Everything else that is foreign is a divergence (the cleat#4157 class; cleat#947 was the await_signals case, below).
func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	carry := s.takeWaitCarry() // cleat#4511: the first wait after an update's completion; cleared here whichever path the call takes
	s.setAwaitRejected(false)
	if s.engine.promiseStore == nil && s.isReplay && s.stepCount < len(s.history) {
		switch s.history[s.stepCount].EventType {
		case EventTypePromiseResolved, EventTypePromiseRejected, EventTypeAwaitPromise:
		default:
			return s.awaitPromiseWithoutAStore(ctx, m, resultPtr, resultMaxLen)
		}
	}
	return s.step(ctx, m, &awaitPromiseStep, stepArgs{s1: promiseID, i1: timeoutMs, carry: carry, outPtr: resultPtr, outMaxLen: resultMaxLen})
}

// awaitPromiseIdentity is the identity check of await_promise: the record must name the promise being awaited, and must be the await's OWN
// outcome, one WITHOUT the cleat#4079 Service/Op discriminator. resolve_promise and reject_promise record the same event types for the same
// promise, and only Service="promise" with Op="resolve" or "reject" tells a settlement the workflow made itself from the answer to an await
// (cleat#4178, which closed it for the record AFTER an await; the first record used to escape it). It is the reverse of settlementIdentity, which
// is the settling call's check and accepts a record with no discriminator.
func awaitPromiseIdentity(a stepArgs, rec *EventRecord) string {
	if rec.PromiseID != a.s1 {
		return fmt.Sprintf("promise %q, recorded %q", a.s1, rec.PromiseID)
	}
	if rec.Service != "" || rec.Op != "" {
		return fmt.Sprintf("an await's own record, recorded as %s/%s (a settlement the workflow made itself)", rec.Service, rec.Op)
	}
	return ""
}

// awaitPromiseStep is AwaitPromise. Its history is, per call, one of:
//
//	[promise_resolved | promise_rejected]            a first read found the promise settled: no await record is written for it
//	[await]                                          the await timed out, or was a no-wait poll that found nothing (cleat#4144); also a history that ENDS here
//	[await, promise_resolved | promise_rejected]     the await was recorded, then answered
//
// The same shape as await_signals, and Identity is what separates the runs: an await naming another promise, or an outcome naming another
// promise or carrying the discriminator, is the next call's record and ends the run (an await that timed out, cleat#4177 and #4178). MaxPrefix is 1
// because a poll loop records [await, await, ...] for the SAME promise. A no-wait call whose recorded await is also no-wait is a run of ONE record,
// so a resolution after it is the next call's (cleat#4144).
//
// What the move tightened, each named and pinned by an_await_promise_move_tightens_only_these_test.go (the rule: identity rows only tighten):
//   - a recorded outcome naming ANOTHER promise, or a settlement the workflow made itself, was consumed and handed back as this await's answer when
//     it was the FIRST record; it is a divergence now. The look-ahead after an await always compared both.
//   - a recorded await naming another promise was consumed as this one's; it is a divergence.
//   - a first record of another type, which was not a divergence: the call fell through to the fresh path with history still unread and replay still
//     on, and recorded at a step that already held an event (the cleat#4157 class; the await_signals analogue was cleat#947). It is a divergence, except for the engine with no
//     promise store (AwaitPromise, above).
//
// A TIMED-OUT AWAIT FOLLOWED BY A SECOND AWAIT THAT FINDS THE PROMISE SETTLED (cleat#4277). Until FirstRead, [await_promise p, promise_resolved p] was
// both what ONE await records when it is answered on a wake and what a timed-out await followed IMMEDIATELY by a second await of the already-settled
// promise records, and a replay handed the first await the resolution where the original run had been told it timed out (an await followed by anything
// else, which is every shape a retry loop makes, was never ambiguous). The second await's settlement, the one its FIRST read of the store produces, is now
// recorded with FirstRead, and OpensACall ends the first await's run before it: the first replays as the timeout it was and the second as the resolution.
// A history written before that field has no marker, so such a pair still replays as it always did, resolved: that is a limit of the old format and not
// a guess, and it converges unless the timeout branch kept guest-local state.
//
// NEVER REFUSED: Refuse is neverRefuse. The old code had no stopBeforeNewWork check, so a defer body, a worker that is shutting down and a session
// that has already suspended all recorded the await and suspended as usual (four rows of the characterization table), and that is kept.
// Whether any of them should be refused is a separate decision.
var awaitPromiseStep = stepSpec{
	Name:               "await_promise",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	// resultLen 32-63, timedOut 16-31, errCode 0-15: the divergence decodes exactly as the simple layout (cleat_await_promise)
	Result:           layoutSimple,
	Effect:           effectEffect,
	Records:          recordsMany,
	Want:             EventTypePromiseResolved,
	EndsWithoutFinal: true,
	MaxPrefix:        1,
	Refuse:           neverRefuse,
	Identity:         awaitPromiseIdentity,
	// A settlement that an await wrote because its FIRST read found the promise settled belongs to the call that wrote it. After an await it is the
	// NEXT call's answer, and the await before it timed out (cleat#4277).
	OpensACall: func(rec *EventRecord) bool {
		return rec.FirstRead && (rec.EventType == EventTypePromiseResolved || rec.EventType == EventTypePromiseRejected)
	},
	Role: func(a stepArgs, rec *EventRecord) recordRole {
		switch rec.EventType {
		case EventTypePromiseResolved, EventTypePromiseRejected:
			return roleFinal
		case EventTypeAwaitPromise:
			if a.i1 <= 0 && rec.TimeoutMs <= 0 {
				// A no-wait await replays as the timeout it was, from the record alone (cleat#4144). Consulting the store, or a resolution that
				// follows, returned RESOLVED to a replay that ran after the promise settled where the original run had been told it timed out. The
				// guest's own argument must also be non-positive, so a recorded 0 that meant "unknown" (written before the timeout was persisted)
				// and is resumed with a positive argument still takes the waiting path.
				return roleFinal
			}
			return rolePrefix
		}
		return roleForeign
	},
	DecodeRun: func(c *stepCtx, run []EventRecord) int64 {
		last := run[len(run)-1]
		switch last.EventType {
		case EventTypePromiseResolved:
			written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, last.PromiseResult, c.args.outMaxLen)
			return packAwaitPromiseResult(written, false, uint16(writtenEC))
		case EventTypePromiseRejected:
			written, _ := c.s.writeResult(c.ctx, c.m, c.args.outPtr, last.PromiseError, c.args.outMaxLen)
			c.s.setAwaitRejected(true)
			return packAwaitPromiseResult(written, false, 1)
		}
		// An await with nothing after it that answers it: history CONTINUES past it with something else, and a pending await can only be left in
		// two ways, both of which leave a trace (cleat#4177). It RESOLVED or was REJECTED, and the outcome is the record right after it; or it TIMED
		// OUT, and nothing is recorded for a timeout ("the deadline has passed" is monotone), so what follows is the guest's next call. The store is
		// NOT consulted: leaving replay here ran every durable call recorded after the await a second time on every later replay.
		return packAwaitPromiseResult(0, true, 0)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, m := c.s, c.ctx, c.m
		promiseID, timeoutMs := c.args.s1, c.args.i1
		reason := fmt.Sprintf("await_promise(%s)", promiseID)
		timedOut := packAwaitPromiseResult(0, true, 0)

		// With no store there is nothing to await: say so and record nothing. Declared, because a step that records nothing and is not a refusal
		// would otherwise read as a spec that did not do what it declared.
		if s.engine.promiseStore == nil {
			c.RecordsNothing("no promise store: nothing to await, and the guest is told so (IMPROVEMENT-PLAN 3.231)")
			return s.awaitPromiseWithoutAStore(ctx, m, c.args.outPtr, c.args.outMaxLen)
		}

		// deliver is the ONE read of the promise store an await makes, on the first poll, at the replay frontier and after a wake. A settled promise is
		// recorded and THEN delivered. A store error is read as still pending, as it always was. firstRead is true only for the first poll, which writes
		// no await record of its own, and marks what it records (cleat#4277). It is a closure and not a function because every Record call must sit
		// inside a spec's Fresh.
		deliver := func(firstRead bool) (int64, bool) {
			status, result, errMsg, err := s.engine.promiseStore.GetPromise(ctx, s.workflowID, promiseID)
			if err != nil {
				return 0, false
			}
			switch status {
			case "resolved":
				c.Record(EventRecord{Step: s.stepCount, EventType: EventTypePromiseResolved, PromiseID: promiseID, PromiseResult: result, FirstRead: firstRead})
				written, writtenEC := s.writeOut(ctx, m, c.args.outPtr, result, c.args.outMaxLen)
				return packAwaitPromiseResult(written, false, uint16(writtenEC)), true
			case "rejected":
				c.Record(EventRecord{Step: s.stepCount, EventType: EventTypePromiseRejected, PromiseID: promiseID, PromiseError: errMsg, FirstRead: firstRead})
				written, _ := s.writeResult(ctx, m, c.args.outPtr, errMsg, c.args.outMaxLen)
				s.setAwaitRejected(true)
				return packAwaitPromiseResult(written, false, 1), true
			}
			return 0, false
		}
		// Every read but the FIRST answers an await that was already recorded, and records its answer unmarked: that pair is one call (cleat#4277).
		deliverIfSettled := func() (int64, bool) { return deliver(false) }

		if len(c.Prefix) > 0 {
			// The history ENDED INSIDE this run: the await was recorded and not answered, and replay has just ended. The promise may have settled
			// since, so ask; a settled one is recorded and delivered. If not, the await keeps waiting against its RECORDED deadline, which
			// belongs to the ORIGINAL await: this wake is measured against when the await began and not against itself (cleat#814, where the
			// deadline was recomputed from the current instant on every wake and could never be reached).
			if r, ok := deliverIfSettled(); ok {
				return r
			}
			rec := c.Prefix[0]
			if rec.TimeoutMs <= 0 {
				// An await recorded before the timeout was persisted, or one with no timeout at all. There is nothing to measure against, so it
				// keeps waiting from now for the guest's own timeout rather than inventing a deadline, and does not re-record the await. That
				// deadline is INVENTED, so it is never handed to a wake source: a wait parked against a deadline of about now would time out on
				// its first return, where this has always suspended (cleat-review3, #4249).
				req := parkRequest{
					Kind: wakePromise, Key: promiseID, Reason: reason,
					DeadlineMs: s.engine.realNowMs() + clampGuestMs(timeoutMs), Until: time.Now().Add(guestMsDuration(timeoutMs)),
				}
				s.suspendFor(req)
				return s.suspendingAwaitResult()
			}
			deadlineMs := rec.TimestampMs + clampGuestMs(rec.TimeoutMs) // clamp: i64::MAX wrapped this negative (cleat#4105)
			// The real clock, not the cached one (s.nowMs): the latter is refreshed on poll cycles and lags the one the scheduler wakes on, which
			// turned this comparison false while the scheduler saw the deadline as past. The workflow was woken at once, re-suspended against the
			// same past deadline and spun (measured: about 34 wakes a second for 91 seconds).
			if s.engine.realNowMs() >= deadlineMs {
				// Timed out. No event is recorded and none is needed: "the deadline has passed" is monotone, so every later replay reaching this
				// point decides the same way. This is the one return here that IS a timeout; a suspending await must not hand the guest a word it
				// reads as one (cleat#3383).
				return timedOut
			}
			return awaitSuspendOrPark(c, deliverIfSettled, parkRequest{Kind: wakePromise, Key: promiseID, DeadlineMs: deadlineMs, Reason: reason, Until: time.UnixMilli(deadlineMs)}, timedOut)
		}

		// Fresh: the store first, so a promise that is already settled is returned and no await is recorded for it.
		// MARKED: no await of this call precedes the record, so a replay must not pair it with an await that came before (cleat#4277).
		if r, ok := deliver(true); ok {
			return r
		}
		// A timeout of 0 (or less) has no deadline to expire, so it must not suspend (cleat#4144, the cleat#1331 loop that await_signals guards
		// against): a deadline of NOW makes the run immediately re-claimable and it wakes, replays to the recorded await and suspends again,
		// forever. Placed AFTER the settled check, so an already-settled promise is still reported and a 0 ms await degrades to a poll. It RECORDS the
		// await with the timeout it was given, because a replay must return the same answer without asking the store. A poll is an event like any
		// other, so it counts toward the event cap and stops at it (cleat#4153).
		if timeoutMs <= 0 {
			if s.refuseAtEventCap(c) {
				return s.suspendingAwaitResult()
			}
			c.Record(EventRecord{Step: s.stepCount, EventType: EventTypeAwaitPromise, PromiseID: promiseID, TimeoutMs: timeoutMs})
			return timedOut
		}
		// First time: record the await WITH its timeout, so a later wake can measure against the original deadline, then wait.
		//
		// The first wait after an update that interrupted a wait on this promise keeps THAT wait's deadline (cleat#4511, waitCarry): the guest called once.
		recTimeoutMs, recStampMs, carriedUntil, carried := c.carriedWait(EventTypeAwaitPromise, func(k string) bool { return k == promiseID }, timeoutMs)
		if !carried {
			recTimeoutMs, recStampMs = timeoutMs, 0
		}
		c.Record(EventRecord{Step: s.stepCount, EventType: EventTypeAwaitPromise, PromiseID: promiseID, TimeoutMs: recTimeoutMs, TimestampMs: recStampMs})
		// AFTER the record, as it always was (see await_signals): the clock moves with the record.
		until := time.UnixMilli(s.nowMs).Add(guestMsDuration(timeoutMs))
		if carried {
			until = carriedUntil
		}
		return awaitSuspendOrPark(c, deliverIfSettled, parkRequest{Kind: wakePromise, Key: promiseID, DeadlineMs: until.UnixMilli(), Reason: reason, Until: until}, timedOut)
	},
}

func init() { registerStepSpec(&awaitPromiseStep) }

// awaitPromiseWithoutAStore is the answer to an await on an engine with no promise store: with none there is nothing to await and nothing that
// could ever resolve it, so falling through to a suspend would wait forever. Say so instead (IMPROVEMENT-PLAN 3.231). It records nothing.
func (s *execSession) awaitPromiseWithoutAStore(ctx context.Context, m api.Module, resultPtr, resultMaxLen uint32) int64 {
	s.engine.log().ErrorContext(ctx, "await_promise: no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
	written, _ := s.writeResult(ctx, m, resultPtr, errNoPromiseStore, resultMaxLen)
	return packAwaitPromiseResult(written, false, 1)
}

// The discriminator values ResolvePromise and RejectPromise stamp on the events they record (cleat#4079). They are exported
// constants, not literals, because a stop point is chosen by reading them back (isStoppableEvent) and two spellings of one
// word would make the stop silently skip every effect.
const (
	PromiseEventService = "promise"
	PromiseOpResolve    = "resolve"
	PromiseOpReject     = "reject"
)

// settlementDecode is the replay answer of resolve_promise and reject_promise: success, unless the event says the store refused the write (cleat#4335).
// The guest's result carries no message for these two (packSimpleResult(1, 0)), so the event's Err only decides the status.
func settlementDecode(_ *stepCtx, rec *EventRecord) int64 {
	if rec.Err != "" {
		return packSimpleResult(1, 0)
	}
	return 0
}

// settleStore performs the store write of a settlement and returns the message to record when it failed, "" when it did not. A missing store is a
// failure like a refusing one (errNoPromiseStore), which is recorded and answered the same way.
func settleStore(c *stepCtx, op string, write func() error) string {
	s, ctx := c.s, c.ctx
	if s.engine.promiseStore == nil {
		s.engine.log().ErrorContext(ctx, op+": no promise store configured", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		return errNoPromiseStore
	}
	if err := write(); err != nil {
		s.engine.log().ErrorContext(ctx, op+" failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return err.Error()
	}
	return ""
}

func (s *execSession) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return s.step(ctx, m, &resolvePromiseStep, stepArgs{s1: promiseID, s2: value})
}

// resolvePromiseStep is ResolvePromise as a step (cleat#4152). Before the identity check (cleat#4211) its replay matched by event TYPE alone,
// so a record naming ANOTHER promise was accepted, consumed and answered with success; it now compares the promise id, and the Service/Op
// discriminator when the record carries one (settlementIdentity). A promise_resolved that carries no Service/Op is still accepted for a
// matching id, which a characterization row keeps visible (resolve_promise/replay_by_type_alone). The discriminator tells a settlement the
// workflow made from the one an await recorded for an already-settled promise; the stop point reads it (cleat#4079), and replay does not
// insist on it.
//
// The refusal the dispatcher applies: a defer segment's body must not settle a promise, that is an effect another run may be waiting on, and
// a segment exists to run cleanup, not to make new calls (cleat#4079, the same refusal DeleteCron got in cleat#4083). It is decided before
// anything is recorded, so a refused call leaves no event.
var resolvePromiseStep = stepSpec{
	Name:               "resolve_promise",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypePromiseResolved,
	Identity: func(a stepArgs, rec *EventRecord) string {
		return settlementIdentity(a.s1, rec, PromiseOpResolve)
	},
	Decode: settlementDecode,
	Fresh: func(c *stepCtx) int64 {
		s, ctx, promiseID, value := c.s, c.ctx, c.args.s1, c.args.s2

		// The store first, then the event with its outcome: see createPromiseStep for why, and for what the order does to the crash window.
		// Reported, not logged-and-swallowed: cleat_resolve_promise's adapter already decodes `errCode := uint32(result)` and turns a non-zero
		// into an error, so the ABI has always had somewhere to put this. A resolve that silently did nothing leaves every awaiter suspended.
		storeErr := settleStore(c, "resolve_promise", func() error { return s.engine.promiseStore.ResolvePromise(ctx, promiseID, value) })
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
			Err:           storeErr,
		})
		if storeErr != "" {
			return packSimpleResult(1, 0)
		}
		return 0
	},
}

func init() { registerStepSpec(&resolvePromiseStep) }

func (s *execSession) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return s.step(ctx, m, &rejectPromiseStep, stepArgs{s1: promiseID, s2: errMsg})
}

// rejectPromiseStep is RejectPromise as a step (cleat#4152). Refused in a defer segment's body, for the reason resolvePromiseStep gives;
// its replay compares the promise id and the Service/Op when present, as resolvePromiseStep does, and before cleat#4211 matched by event
// type alone.
var rejectPromiseStep = stepSpec{
	Name:               "reject_promise",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypePromiseRejected,
	Identity: func(a stepArgs, rec *EventRecord) string {
		return settlementIdentity(a.s1, rec, PromiseOpReject)
	},
	Decode: settlementDecode,
	Fresh: func(c *stepCtx) int64 {
		s, ctx, promiseID, errMsg := c.s, c.ctx, c.args.s1, c.args.s2

		// The store first, then the event with its outcome (see createPromiseStep). Service and Op: see resolvePromiseStep.
		storeErr := settleStore(c, "reject_promise", func() error { return s.engine.promiseStore.RejectPromise(ctx, promiseID, errMsg) })
		c.Record(EventRecord{
			Step:         s.stepCount,
			EventType:    EventTypePromiseRejected,
			Service:      PromiseEventService,
			Op:           PromiseOpReject,
			PromiseID:    promiseID,
			PromiseError: errMsg,
			Err:          storeErr,
		})
		if storeErr != "" {
			return packSimpleResult(1, 0)
		}
		return 0
	},
}

func init() { registerStepSpec(&rejectPromiseStep) }

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
