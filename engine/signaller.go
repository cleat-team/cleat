package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// DurableAwaitSignals is a step (cleat#4152, W5): the dispatcher owns where the call is in history, whether it may start new work, and what a
// replay consumes; awaitSignalsStep says what the call is. It used to carry all of that in one 350-line function whose comments were the record of
// nine bugs (cleat#933, #947, #975, #1331, #3378, #4105, #4144, #4153, #4157); each is kept next to the rule it produced, below.
func (s *execSession) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	return s.step(ctx, m, &awaitSignalsStep, stepArgs{
		s1: signalNames, i1: timeoutMs,
		// A divergence message goes to the payload buffer, as it always did; the signal's name has its own.
		outPtr: payloadPtr, outMaxLen: payloadMaxLen,
		out2Ptr: sigNamePtr, out2MaxLen: sigNameMaxLen,
	})
}

// awaitSignalsStep is DurableAwaitSignals. Its history is, per call, one of:
//
//	[signal_received]            a first poll found a delivery: no await record is written for a poll that answered at once
//	[await]                      the await timed out, or was a no-wait poll that found nothing (cleat#4144); also a history that ENDS here
//	[await, signal_received]     the await was recorded, then answered
//
// Identity is what makes the runs separable. The await's names must be the call's, and a delivery's name must be one it asked for: without that
// a delivery recorded for another await was consumed by this one (the misattribution cleat#933 describes), and a run of timed-out awaits could
// not be told apart from the next call's records (validate() requires an Identity for EndsWithoutFinal). That is a TIGHTENING: the old code
// compared nothing. MaxPrefix is 1 because a polling loop that times out records [await, await, ...] with the SAME names each time, which no
// Identity can separate.
//
// A no-wait call (timeout <= 0) whose recorded await is also no-wait is a run of ONE record, so the delivery that follows it belongs to the next
// call and is never paired (cleat#4144: the poll loop's [await, delivery] is two calls). That is the Role's dependence on the call's argument.
var awaitSignalsStep = stepSpec{
	Name:             "await_signals",
	Effect:           effectEffect,
	Records:          recordsMany,
	Want:             EventTypeSignalReceived,
	EndsWithoutFinal: true,
	MaxPrefix:        1,
	Identity: func(a stepArgs, rec *EventRecord) string {
		asked := parseSignalNames(a.s1)
		switch rec.EventType {
		case EventTypeAwaitSignals:
			if got := parseSignalNames(rec.SignalNames); !sameSignalNames(got, asked) {
				return fmt.Sprintf("signals %v, recorded %v", asked, got)
			}
		case EventTypeSignalReceived:
			for _, n := range asked {
				if n == rec.SignalName {
					return ""
				}
			}
			return fmt.Sprintf("signals %v, recorded a delivery of %q", asked, rec.SignalName)
		}
		return ""
	},
	Role: func(a stepArgs, rec *EventRecord) recordRole {
		switch rec.EventType {
		case EventTypeSignalReceived:
			return roleFinal
		case EventTypeAwaitSignals:
			if a.i1 <= 0 && rec.TimeoutMs <= 0 {
				return roleFinal // a no-wait poll that found nothing: a run of one, the answer is the timeout
			}
			return rolePrefix
		}
		return roleForeign
	},
	DecodeRun: func(c *stepCtx, run []EventRecord) int64 {
		last := run[len(run)-1]
		if last.EventType != EventTypeSignalReceived {
			// An await with nothing after it that answers it: history CONTINUES past it with something that is not a delivery, so the original
			// execution reached that event without receiving a signal, and the await timed out. The store is NOT consulted: an event can only
			// land at the frontier, so there is nowhere to put a delivery that belongs in the middle of a written history (cleat#947: polling
			// here handed the guest a signal the original run never saw and wrote a record that collided with the one already at that step).
			return packAwaitSignalsResult(0, 0, true, 0)
		}
		return awaitDelivery(c, last.SignalName, last.SignalPayload)
	},
	// A history written before cleat#4151 recorded nothing for a no-wait poll that timed out (cleat#1331), so the record at this step is the
	// NEXT call's. A no-wait poll's answer is the timeout, and the record is left for the call it belongs to. A build that has #4151 records every
	// such poll, so it never gets here.
	ForeignFirst: func(a stepArgs) (int64, bool) {
		if a.i1 <= 0 {
			return packAwaitSignalsResult(0, 0, true, 0), true
		}
		return 0, false
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, m := c.s, c.ctx, c.m
		names, timeoutMs := c.args.s1, c.args.i1

		// deliverIfWaiting is the ONE read of the signal store an await makes, on the first poll, at the replay frontier and after a wake: it polls
		// the asked names in order, and a delivery found is recorded and THEN consumed (so a crash between them leaves the delivery for the next
		// poll, never a consumed delivery with no record), then written to the guest. A store error is read as nothing found. It is a closure here,
		// and not a function, because every Record call must sit inside a spec's Fresh: that is what the dispatcher's exemption for Record says.
		deliverIfWaiting := func() (int64, bool) {
			if s.engine.signalStore == nil {
				return 0, false
			}
			for _, name := range parseSignalNames(names) {
				d, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, name)
				if err == nil && found {
					c.Record(EventRecord{Step: s.stepCount, EventType: EventTypeSignalReceived, SignalName: name, SignalPayload: d.Payload})
					s.consumeDelivered(ctx, name, d)
					return awaitDelivery(c, name, d.Payload), true
				}
			}
			return 0, false
		}

		// A SECOND AWAIT IN A SESSION THAT HAS ALREADY SUSPENDED IS ALSO NEW WORK (cleat#933): a suspending await's result used to be
		// byte-identical to a timeout, the guest ran on, and the next await recorded a second await_signals into a segment that had already ended.
		// Since cleat#3378 the suspending await returns the stop, so this is the backstop for a guest that ignores it, and for the defer drain.
		// Scoped to awaits deliberately: widening it to every call refused the cleanup a workflow registered at a continue-as-new boundary.
		if s.suspendErr != nil && !s.inDeferDrain {
			return callSuspendSentinel
		}

		if len(c.Prefix) > 0 {
			// The history ENDED INSIDE this run: the await was recorded and not answered, and replay has just ended. Replay ends here, and
			// saying so is load-bearing: polling the store and consuming a delivery is new work, and recordEvent persists differently on the
			// two sides of that line (a signal_received recorded with replay still on skipped the checksum chain: cleat#933's
			// "step 2: checksum mismatch").
			if r, ok := deliverIfWaiting(); ok {
				return r
			}
			// Still nothing. The await has NOT resolved, so it must suspend, not report a timeout that has not happened (cleat#933). The
			// deadline comes from the RECORDED await, not from now: deriving it from the clock would push the timeout out on every replay and
			// the await would never expire. A deadline already passed is a genuine timeout. A record with no timestamp, or no timeout, has no
			// usable deadline, and rec.TimestampMs + rec.TimeoutMs would be a few seconds past the epoch, so it would time out instantly.
			rec := c.Prefix[0]
			deadline := rec.TimestampMs + clampGuestMs(rec.TimeoutMs)
			if rec.TimestampMs > 0 && rec.TimeoutMs > 0 && s.engine.realNowMs() >= deadline {
				return packAwaitSignalsResult(0, 0, true, 0)
			}
			if rec.TimestampMs == 0 || rec.TimeoutMs <= 0 {
				deadline = s.engine.realNowMs() + clampGuestMs(rec.TimeoutMs)
			}
			return awaitSuspendOrPark(c, deliverIfWaiting, parkRequest{
				Kind: wakeSignal, Key: names, DeadlineMs: deadline,
				Reason: fmt.Sprintf("await_signals(%s, %dms) [replayed]", rec.SignalNames, rec.TimeoutMs),
				Until:  time.UnixMilli(deadline),
			})
		}

		// Fresh: the store first, so a delivery that is already waiting is returned and no await is recorded for it.
		if r, ok := deliverIfWaiting(); ok {
			return r
		}
		// A 0ms await has no deadline to expire, so it must not suspend (cleat#1331): a deadline of NOW makes the run immediately re-claimable and
		// it livelocks at about fourteen claims a second. Guarded here as well as in the SDK because this is on the public interface and a guest
		// that computes a sub-millisecond timeout arrives here with 0. It RECORDS the await (cleat#4144, #4153): the constant it returns is the
		// outcome of a look at the store, and a replay that reached this call after a signal arrived would otherwise return the signal where the
		// original returned a timeout. A poll loop therefore records one event per iteration, each counting toward the event cap.
		if timeoutMs <= 0 {
			if s.refuseAtEventCap(ctx, m) {
				return s.suspendingAwaitResult()
			}
			c.Record(EventRecord{Step: s.stepCount, EventType: EventTypeAwaitSignals, SignalNames: names, TimeoutMs: timeoutMs})
			return packAwaitSignalsResult(0, 0, true, 0)
		}
		// Record the await, then wait: it is written BEFORE the call blocks or suspends, once, and never per iteration of a park.
		c.Record(EventRecord{Step: s.stepCount, EventType: EventTypeAwaitSignals, SignalNames: names, TimeoutMs: timeoutMs})
		until := time.UnixMilli(s.nowMs).Add(guestMsDuration(timeoutMs))
		return awaitSuspendOrPark(c, deliverIfWaiting, parkRequest{
			Kind: wakeSignal, Key: names, DeadlineMs: until.UnixMilli(),
			Reason: fmt.Sprintf("await_signals(%s, %dms)", names, timeoutMs),
			Until:  until,
		})
	},
}

// awaitSuspendOrPark is the wait: park if this session may and there is a wake source, else suspend (production today: every wait suspends).
//
// What a park returns is ADVISORY, exactly as for a durable sleep (cleat#4237): a wake and a deadline both carry no payload, a delivery can land
// with either, and a source can be wrong about the time. So after EVERY return from the source the call polls the store once, through the same read
// the first poll used; it is a timeout only if the engine's real clock agrees the deadline has passed; and a deadline outcome the clock does not
// agree with suspends, so the scheduler's own clock decides when the run wakes. Trusting the outcome returned a timeout without looking, which lost
// a delivery that landed with the deadline (it was then delivered only by suspend and replay) and timed a wait out at once on a source that was early.
// The clock check also gives an empty wake loop an exit that does not depend on the source. The real clock, not the cached one (s.nowMs), because
// the scheduler wakes on real time: the same reasoning as AwaitPromise's deadline.
//
// Baseline is 0: the lost-wakeup window is closed by the wake source's own ordering (it registers, then the call re-polls), so no sequence is
// compared in v1 (WS-cleat-2, #4152).
func awaitSuspendOrPark(c *stepCtx, deliverIfWaiting func() (int64, bool), req parkRequest) int64 {
	for {
		out := c.s.parkOrSuspend(c.ctx, req)
		if out == parkRefused {
			return c.s.suspendingAwaitResult()
		}
		if r, ok := deliverIfWaiting(); ok {
			return r
		}
		if c.s.engine.realNowMs() >= req.DeadlineMs {
			return packAwaitSignalsResult(0, 0, true, 0)
		}
		if out == parkDeadline {
			c.s.suspendFor(req)
			return c.s.suspendingAwaitResult()
		}
	}
}

// awaitDelivery writes a delivery's name and payload to the guest. Either write can overflow, and the first non-zero code wins: a caller that
// sees one truncation does not need to know there were two, and reporting success because only the payload was cut would be the defect this
// replaced.
func awaitDelivery(c *stepCtx, name, payload string) int64 {
	written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.out2Ptr, name, c.args.out2MaxLen)
	payloadWritten, payloadEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, payload, c.args.outMaxLen)
	return packAwaitSignalsResult(written, payloadWritten, false, uint32(firstNonZero(writtenEC, payloadEC)))
}

func sameSignalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// suspendingAwaitResult is what an await that has just suspended the run hands
// back to the guest: callSuspendSentinel, which every SDK's await_signals
// binding reads as a stop before decoding any field -- Go panics ErrSuspend,
// Rust returns Err(Suspended), Java throws, Python raises, AssemblyScript sets
// its stop flag (IMPROVEMENT-PLAN 3.202). cleat#3378.
//
// It used to return packAwaitSignalsResult(0, 0, true, 0), on the belief that
// setting suspendErr ended the segment before the guest could observe the
// value. It did not. That word is byte-identical to a genuine timeout, the
// guest ran on down its timeout branch, and the first call there that did not
// check suspendErr executed: a PluginCall delivered a real webhook 5ms into a
// 120s wait, and its plugin_call record then committed every replay to the
// same false timeout. The only call that did stop it was a second await
// (cleat#933's guard below), which is why a loop of awaits looked fine.
//
// EXCEPT INSIDE THE HOST-RUN DEFER DRAIN, for the reason the cleat#933 guard
// exempts it: there the host is running the guest's cleanup itself, and a stop
// raised from inside a defer body would abandon the cleanup after it. The run
// still suspends -- suspendErr is set either way.
func (s *execSession) suspendingAwaitResult() int64 {
	if s.inDeferDrain {
		return packAwaitSignalsResult(0, 0, true, 0)
	}
	return callSuspendSentinel
}

func (s *execSession) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	if s.isReplay {
		return 0 // never cancelled during replay
	}

	if s.engine.signalStore != nil {
		cancelled, reason, err := s.engine.signalStore.PollCancellation(ctx, s.engine.workflowID)
		if err == nil && cancelled {

			// The written count, not len(reason): writeResult truncates to
			// reasonMaxLen, and reporting the untruncated length tells the
			// guest to read past what was written. Same defect as the signal
			// payload one call up, and the same fix.
			reasonWritten, _ := s.writeResult(ctx, m, reasonPtr, reason, reasonMaxLen)
			return int64(uint64(reasonWritten)<<32 | 1) // cancelled=true
		}
	}
	return 0
}

// PollSignal is the guest's non-durable peek: it reports whether a signal is
// waiting and does NOT consume it, so a workflow can poll in a loop without
// draining the queue. Only the await paths consume, and only after recording
// the event that makes the consumption replayable.
// PollSignal is the non-blocking counterpart to DurableAwaitSignals: it answers
// immediately rather than suspending.
//
// It records no event, and does not need to. The rule replay requires is not
// "everything writes an event" -- DurableSleep is deterministic and stores
// nothing -- but "the answer is a function of recorded state". A signal's
// delivered_at is recorded state, and the session's durable clock is too, so
// the answer is derived from the two. That is the same resolution #847 reached
// for PollChild, arrived at independently and for the same reason.
//
// Before this, PollSignal re-queried the store live on every execution, with no
// isReplay check at all, unlike SignalWorkflow immediately below it. So a poll
// that answered "nothing" before a suspension answered "yes, here is the
// payload" after the prefix re-ran -- returning a payload that did not exist
// when that line first executed (#882).
//
// A sweep of all 50 host-call entry points found exactly two reading live state
// without replay handling: PollChild and this. The family is closed, confirmed
// independently from the opposite direction -- enumerate everything that reads
// live state, then ask which are entry points, so a call the first enumeration
// missed still surfaces in the second.
//
// The two sweeps quote different totals because they count different things,
// not because either is wrong: 50 is the host-call entry points HostHandler
// declares, 98 is the methods on *execSession, of which 25 touch a store. If
// you meet both numbers below, that is why.
//
// Also naming the comparison, since the instruction "read the durable-time
// comparison instead" points at a different symbol in each: PollChild compares
// completed_at, this method compares DeliveredAtMs, both against s.nowMs.
//
// WARNING TO WHOEVER RUNS THAT SWEEP NEXT, because it will mislead you:
//
// This method and PollChild have NO isReplay check, no history read and record
// no event -- deliberately, because both derive their answer from durable time
// instead. A scan that looks for replay handling by name (isReplay,
// advanceReplayStep, exitReplay) therefore flags the two calls in this codebase
// that solved the problem most thoroughly, and flags them exactly as it flagged
// them when they were broken.
//
// The output looks identical before and after the fix. Do not conclude from it
// that these were reverted; read the comparison against s.nowMs instead.
//
// Recorded here rather than on the closed issue, because a sweep is written by
// someone reading the code, not the tracker.
//
// THE SAME WARNING FROM THE OTHER SIDE, and the more alarming half.
//
// The first version of that sweep hard-coded five store field names and did not
// include childWfStore. That blind spot was not one method, it was SEVEN -- the
// entire child-workflow surface of execSession: AwaitAnyChild, AwaitChild,
// PollChild, RunDetached, childWorkflowWithVersion, freshAwaitAllChildren and
// resolveChildVersion. Verified by counting.
//
// It still produced the right total, because the total was known independently.
// PollChild was already filed as #847 by the person running the sweep -- so the
// sweep was run against a case already known to be broken, DID NOT REPORT IT,
// and was believed anyway because its answer matched what its author already
// thought.
//
// So: one sweep will flag two calls that are fine, and another can miss seven
// and still look right. Neither failure is visible in the output. Give any such
// scan a case you already know it must find, and check that it finds it.
func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	if s.engine.signalStore != nil {
		d, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, signalName)
		if err == nil && found && s.signalIsVisibleNow(d) {
			written, _ := s.writeResult(ctx, m, payloadPtr, d.Payload, payloadMaxLen)
			flags := uint32(0x0100) // found=true
			return int64(uint64(written)<<32 | uint64(flags))
		}
	}
	return 0 // not found
}

// signalIsVisibleNow reports whether a delivery had already arrived as of this
// session's durable clock.
//
// A signal delivered AFTER that instant was not visible to the original
// execution, so no replay may see it either -- that is the whole of #882.
//
// A zero DeliveredAtMs means the store did not populate it, which no real store
// does: the column is NOT NULL DEFAULT now() in all three schemas, and
// TestEveryPollSignalSelectsDeliveredAt holds them to it. Only a test double
// reaches this, and it is treated as visible so doubles keep behaving as they
// did.
//
// The comparison is not exact and cannot be. delivered_at is written by the
// DATABASE clock (now()), while nowMs derives from the WORKER clock via
// rec.TimestampMs -- measured 40-60ms apart on two machines (#804). A signal
// delivered within that window of the poll may fall on either side. What the
// comparison guarantees is the property that matters: the answer depends only
// on two recorded values, so it is the same on every replay. It is
// deterministic, not precise, and those are different claims.
func (s *execSession) signalIsVisibleNow(d SignalDelivery) bool {
	if d.DeliveredAtMs == 0 {
		return true
	}
	return d.DeliveredAtMs <= s.nowMs
}

// SendSignalAndWait and ReplyToSignal lived here until 2026-09-06 and were
// both INERT (IMPROVEMENT-PLAN 3.220). SendSignalAndWait did the replay check,
// the stop guard and the authorization check, then polled the TARGET's queue
// and suspended -- it never called DeliverSignal, so it waited for a reply to
// a message it had not sent. ReplyToSignal recorded a local
// EventTypeSignalReceived named by the correlation ID and wrote nothing
// anywhere.
//
// Request/reply is now an SDK composite in all five languages: a promise is
// the reply channel and its ID is the correlation ID, so the reply address is
// data rather than protocol -- which is how DBOS and Temporal handle it too,
// neither having a primitive for it.
// The send side's event (cleat#4102). A send and a delivery are both recorded as signal_received, and the await path matches on
// that type alone, so the send carries Service and Op to be told apart from a delivery, and its OUTCOME in Response. The outcome is
// what replay returns to the guest, so it is inside the event checksum (eventRecordToPayload) and survives compaction. Response is
// empty for a send that was delivered, which is also what every send recorded before cleat#4102 holds, so an old history replays
// as the success it was reported as.
const (
	SignalEventService = "signal"
	SignalOpSend       = "send"

	signalOutcomeAuthRequired   = "auth_required"
	signalOutcomeDeliveryFailed = "delivery_failed"
	signalOutcomeNotFound       = "not_found"
)

// signalOutcomeCode is the call's result word for a recorded outcome. An outcome this build does not know is a failure and not a
// success: a history written by a newer engine that added one must not be replayed by this one as "delivered".
func signalOutcomeCode(outcome string) int64 {
	switch outcome {
	case "":
		return 0
	case signalOutcomeAuthRequired:
		return errSignalAuthRequiredInt
	case signalOutcomeNotFound:
		return errSignalTargetNotFoundInt
	default:
		return errSignalDeliveryFailedInt
	}
}

// signalSendOutcomeCode reads a replayed signal_received event as the result of a send. An event with no Op is a delivery, or a send
// recorded before cleat#4102, and is the success it always replayed as.
func signalSendOutcomeCode(rec EventRecord) int64 {
	if rec.Service != SignalEventService || rec.Op != SignalOpSend {
		return 0
	}
	return signalOutcomeCode(rec.Response)
}

// deliverSignal hands the signal to the store and classifies what came back. The error text is logged and not recorded: the event
// carries a CLASS, which is all a guest can act on and all that is safe to put in a history an admin can read.
func (s *execSession) deliverSignal(ctx context.Context, targetRunID, signalName, payload string) string {
	if s.engine.signalStore == nil {
		// No store is configured, so there is nowhere to deliver to. This has always reported success, and whether it should is a
		// separate question from the one cleat#4102 asked (a store that REFUSED); engines without a store are tests and embedded
		// development engines, listed in the pull request.
		return ""
	}
	err := s.engine.signalStore.DeliverSignal(ctx, targetRunID, signalName, payload)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrWorkflowNotFound):
		// The same outcome for a target that does not exist and one under another tenant (cleat#2218, cleat#2227): the store already
		// returns one error for both, and this keeps it one outcome. Do not log the target's existence either way.
		s.engine.log().WarnContext(ctx, "deliver_signal: target not found", "workflow_id", s.workflowID, "tenant_id", s.tenantID)
		return signalOutcomeNotFound
	default:
		s.engine.log().ErrorContext(ctx, "deliver_signal failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return signalOutcomeDeliveryFailed
	}
}

// SignalWorkflow sends a signal to another workflow.
//
// ORDER IS THE CONTRACT. The signal is delivered FIRST and its outcome recorded after, which is the opposite of what this did until
// cleat#4102, and the difference is who loses. Recording first (the old order) meant a store error AND a crash between the two
// steps both lost the signal for good: the event said it was sent, replay returns what the event says, and nothing ever tried
// again. Delivering first means a crash between the two re-delivers on replay, which is a DUPLICATE and not a loss. That is
// at-least-once, the stance consumeDelivered already takes for the await side; an idempotency key on the delivery would make it
// effectively-once and is a follow-up.
//
// The outcome is recorded whatever it was, and replay returns the recorded one. Not recording a failure would let the guest see an
// error live, handle it, and carry on, while replay found no event at this step, re-ran the delivery, possibly succeeded, and
// diverged from the history the guest's later events were recorded against.
func (s *execSession) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return signalSendOutcomeCode(rec)
			}
			return s.replayDivergence(ctx, m, EventTypeSignalReceived, 0, 0)
		}
		s.exitReplay()
	}

	// A fresh signal_workflow is new work: it delivers a signal to another
	// workflow, which can wake it and start a run. A defer segment exists to run
	// a terminated workflow's cleanup, not to start something on its behalf.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	outcome := ""
	refused := false
	// Check signal authorization before delivering.
	if s.engine.requireSignalAuth && s.engine.signalAuthCheck != nil {
		if err := s.engine.signalAuthCheck(ctx, targetRunID, s.defName); err != nil {
			s.engine.log().ErrorContext(ctx, "signal_auth failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			outcome = signalOutcomeAuthRequired
			refused = true
		}
	}
	if !refused {
		outcome = s.deliverSignal(ctx, targetRunID, signalName, payload)
	}

	s.recordEvent(EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeSignalReceived,
		Service:       SignalEventService,
		Op:            SignalOpSend,
		Response:      outcome,
		SignalName:    signalName,
		SignalPayload: payload,
		RunID:         targetRunID,
	})
	return signalOutcomeCode(outcome)
}

// consumeDelivered removes a delivery from this workflow's queue, after the
// signal_received event recording it is durable.
//
// The ORDER is the whole point, and it is the difference between at-least-once
// and at-most-once delivery.
//
// recordEvent persists synchronously (engine/lifecycle.go, "Persist immediately
// so events survive worker crashes"), so by the time this is called the fact
// that the workflow received this payload is on disk. A crash here therefore
// leaves the row present and the event recorded: replay reads the payload out
// of history positionally and never re-polls, so the stale row is at worst
// handed to a LATER await of the same name -- a duplicate. Consuming first and
// crashing before the event was durable would instead lose the signal outright,
// with no record anywhere that it ever arrived.
//
// A failure to consume is logged and swallowed for the same reason. The
// workflow has already been told it received the signal, and there is no
// unwinding that; returning an error here would fail a run that succeeded.
func (s *execSession) consumeDelivered(ctx context.Context, name string, d SignalDelivery) {
	s.consumeDeliveredFrom(ctx, s.engine.workflowID, name, d)
}

func (s *execSession) consumeDeliveredFrom(ctx context.Context, workflowID, name string, d SignalDelivery) {
	if s.engine.signalStore == nil {
		return
	}
	if err := s.engine.signalStore.ConsumeSignal(ctx, workflowID, d.ID); err != nil {
		s.engine.log().ErrorContext(ctx, "consume_signal failed; the delivery may be handed out again",
			"workflow_id", workflowID, "tenant_id", s.tenantID, "signal_name", name, "signal_id", d.ID, "error", err)
	}
}

// firstNonZero returns the first non-zero errCode, or 0.
//
// For host calls that write two values into two guest buffers: either can
// overflow, and the call has failed if either did.
func firstNonZero(codes ...byte) byte {
	for _, c := range codes {
		if c != 0 {
			return c
		}
	}
	return 0
}
