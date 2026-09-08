package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				written, _ := s.writeResult(ctx, m, sigNamePtr, rec.SignalName, sigNameMaxLen)
				payloadWritten, _ := s.writeResult(ctx, m, payloadPtr, rec.SignalPayload, payloadMaxLen)
				return packAwaitSignalsResult(written, payloadWritten, false, 0)
			}
			if rec.EventType == EventTypeAwaitSignals {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				// Check if there is a following signal_received event.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeSignalReceived {
						if !s.advanceReplayStep(ctx, &nextRec) {
							return 0
						}
						written, _ := s.writeResult(ctx, m, sigNamePtr, nextRec.SignalName, sigNameMaxLen)
						payloadWritten, _ := s.writeResult(ctx, m, payloadPtr, nextRec.SignalPayload, payloadMaxLen)
						return packAwaitSignalsResult(written, payloadWritten, false, 0)
					}
					// History CONTINUES past this await with something that is
					// not a delivery, so the original execution reached that
					// next event without receiving a signal: the await timed
					// out. Reproduce that, and do not look in the store.
					//
					// The rule is DurableAwaitUpdate's, stated in its doc:
					// "The table is NOT consulted -- a request that arrived
					// later must not be delivered at an earlier step." Its
					// reason applies verbatim here, because it is a property
					// of recordEvent rather than of updates: an event can only
					// land at the frontier, so there is nowhere to put a
					// delivery that belongs in the middle of a written
					// history.
					//
					// Polling here did all four (cleat#947): handed the guest
					// a signal the original run never saw, consumed the
					// delivery so a later legitimate await could not have it,
					// wrote a record whose Step was s.stepCount while the
					// append landed at len(s.history) -- colliding with the
					// event already at that step -- and left s.stepCount one
					// past the record it should read next, so the rest of the
					// replay ran off by one against its own history.
					//
					// Returning timed-out is not a new behaviour: it is what
					// this path already did whenever the store happened to be
					// empty, which is why an empty store was correct and a
					// non-empty one was not.
					return packAwaitSignalsResult(0, 0, true, 0)
				}
				// No signal_received in history. The signal may have
				// arrived after suspend (stored in workflow_signals,
				// not event_history). Check the signal store.
				//
				// REPLAY ENDS HERE, and saying so is load-bearing rather
				// than tidy. History is exhausted at this await, so polling
				// the store and consuming a delivery is new work, not
				// reproduction of old work -- and recordEvent behaves
				// differently on the two sides of that line:
				//
				//	if s.engine.db != nil && !s.isReplay {   // lifecycle.go
				//		... flush ...
				//		s.lastChecksum = checksum
				//	}
				//
				// Recording the signal_received with isReplay still true
				// therefore skipped BOTH halves, and only one of them came
				// back. The ROW did: the worker persists everything
				// appended past the loaded history at segment end
				// (cmd/cleat-worker/setup.go, "newEvents =
				// resultHistory[len(history):]"), so it lands one segment
				// late -- durable eventually, but not at the moment
				// consumeDelivered deletes the delivery, which is the
				// window that comment's ordering argument exists to close.
				//
				// s.lastChecksum did NOT come back, and that is what fails
				// the run. The next event -- the second await, flushed
				// immediately -- was chained from the await BEFORE the
				// signal. Verification recomputes the chain over what is
				// stored, including the row finalize wrote, and the two
				// disagree at exactly the step cleat#933 reports:
				//
				//	verify events: workflow ...: step 2: checksum mismatch
				//
				// Unconditional: the branch above returns for every case where
				// history continues, so reaching here means it is exhausted and
				// the poll below is this run's first fresh act.
				s.exitReplay()
				if s.engine.signalStore != nil {
					names := parseSignalNames(rec.SignalNames)
					for _, name := range names {
						d, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, name)
						if err == nil && found {
							// Record the signal_received event so
							// subsequent replays find it in history,
							// THEN consume. See consumeDelivered.
							sigRec := EventRecord{
								Step:          s.stepCount,
								EventType:     EventTypeSignalReceived,
								SignalName:    name,
								SignalPayload: d.Payload,
							}
							s.recordEvent(sigRec)
							s.consumeDelivered(ctx, name, d)
							written, _ := s.writeResult(ctx, m, sigNamePtr, name, sigNameMaxLen)
							payloadWritten, _ := s.writeResult(ctx, m, payloadPtr, d.Payload, payloadMaxLen)
							return packAwaitSignalsResult(written, payloadWritten, false, 0)
						}
					}
				}
				// No signal found, and the await it belongs to has NOT
				// resolved. It must suspend, exactly as the fresh path does --
				// not report a timeout that has not happened (cleat#933).
				//
				// This returned packAwaitSignalsResult(..., timedOut=true)
				// under the comment "Should not happen (we only wake when
				// signal arrives), but handle gracefully". It does happen, on
				// any replay that reaches the await before the signal arrives,
				// and it is the ordinary case rather than an edge one.
				//
				// THE TWO PATHS RETURN THE SAME WORD AND DIFFER IN WHETHER THE
				// RUN SUSPENDS, which is what made this survive: the fresh path
				// below also returns timedOut=true, but it sets suspendErr
				// first, so the segment ends and the guest never observes the
				// value. Here nothing suspended, so the guest read it as a real
				// timeout and ran on to its next await.
				//
				// That is what wrote the corrupt history cleat#933 reports.
				// Measured, two straight-line awaits and one signal:
				//
				//	step 0  await_signals ["a","b"]
				//	step 1  await_signals ["a","b"]     <- the second await,
				//	step 2  signal_received a              recorded because the
				//	                                       first falsely returned
				//
				// Replaying that pairs the delivery with the SECOND await and
				// times out the first -- the misattribution reported against
				// the real system, faithfully reproduced from a history that
				// should never have been written. The replay arm was innocent;
				// the fault is one segment earlier, here.
				//
				// The deadline comes from the RECORDED await, not from now.
				// Deriving it from the current clock would push the timeout
				// further out on every replay and the await would never expire.
				// A deadline already passed is a genuine timeout, and reporting
				// it is then correct -- the same virtual-versus-real comparison
				// DurableSleep makes before deciding to suspend.
				// A record with no timestamp cannot yield a deadline, and
				// rec.TimestampMs + rec.TimeoutMs would then be a few seconds
				// past the epoch -- already elapsed, so it would report a
				// timeout instantly and rebuild the exact defect above. Every
				// event recordEvent writes carries a timestamp and every row
				// loaded from a store carries created_at, so this is a guard
				// rather than a live case; it is here because the failure mode
				// it prevents is the one being removed. Found by a fixture that
				// omitted TimestampMs, which is how the shape reached me.
				deadline := rec.TimestampMs + rec.TimeoutMs
				expired := rec.TimestampMs > 0 && rec.TimeoutMs > 0 &&
					s.engine.realNowMs() >= deadline
				if expired {
					return packAwaitSignalsResult(0, 0, true, 0)
				}
				if rec.TimestampMs == 0 || rec.TimeoutMs <= 0 {
					// No usable deadline: suspend without one and let the
					// worker's own scheduling decide when to look again.
					deadline = s.engine.realNowMs() + rec.TimeoutMs
				}
				s.suspendErr = &SuspendError{
					Reason: fmt.Sprintf("await_signals(%s, %dms) [replayed]",
						rec.SignalNames, rec.TimeoutMs),
					Until: time.UnixMilli(deadline),
				}
				return packAwaitSignalsResult(0, 0, true, 0)
			}
		}
		s.exitReplay()
	}

	// A fresh await is new work: it records an await_signals event and suspends
	// the run, so a defer segment that reached this point would leave the
	// terminated workflow waiting for a signal instead of finishing its
	// cleanup. See IMPROVEMENT-PLAN 3.84.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	// A SECOND AWAIT IN A SESSION THAT HAS ALREADY SUSPENDED IS ALSO NEW WORK,
	// and missing that is cleat#933.
	//
	// A suspending await sets s.suspendErr and returns
	// packAwaitSignalsResult(0, 0, true, 0) -- byte-identical to a genuine
	// timeout. The guest cannot tell the two apart, so it runs on and the next
	// await records a second await_signals into a segment that has already
	// ended. Measured on a fresh session with an empty store:
	//
	//	step 0  await_signals  ts=1788874160927
	//	step 1  await_signals  ts=1788874160927   <- same millisecond
	//
	// which is the history the samples-go port dumps from a real run: two
	// awaits recorded three seconds before the signal existed. The next replay
	// then pairs the delivery with the SECOND await and times out the first.
	// The replay arm reproduces that faithfully; the fault is that the history
	// was written.
	//
	// SCOPED TO AWAITS DELIBERATELY. The same session will happily run a
	// DurableCall or a SideEffect after a suspend, and widening
	// stopBeforeNewWork to cover them looked like the general fix -- it is not,
	// and two tests said so. At a continue-as-new boundary the GUEST drains its
	// own defer table by returning through its wrapper, with no inDeferDrain
	// bracket to distinguish it, so gating every call refused the cleanup
	// (TestTheEventCapDoesNotDispatchTheCallItRefused, "the cleanup a workflow
	// registered did not run"). A second await is unambiguous in a way a
	// second call is not: nothing legitimate awaits a signal in a segment that
	// has already decided to end. The broader case is filed rather than
	// guessed at.
	if s.suspendErr != nil && !s.inDeferDrain {
		return callSuspendSentinel
	}

	// Fresh execution: check signal store first.
	if s.engine.signalStore != nil {
		// parseSignalNames, not splitSignalNames: the guest sends a JSON
		// array, and the comma splitter turned `["a","b"]` into `["a` and
		// `"b]`, so this poll could never match and a signal already waiting
		// was missed. The replay arm has always parsed it as JSON -- one
		// field read two ways was the defect, so both arms now call one
		// helper rather than two that are meant to agree.
		names := parseSignalNames(signalNames)
		for _, name := range names {
			d, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, name)
			if err == nil && found {
				rec := EventRecord{
					Step:          s.stepCount,
					EventType:     EventTypeSignalReceived,
					SignalName:    name,
					SignalPayload: d.Payload,
				}
				s.recordEvent(rec)
				s.consumeDelivered(ctx, name, d)

				written, _ := s.writeResult(ctx, m, sigNamePtr, name, sigNameMaxLen)
				payloadWritten, _ := s.writeResult(ctx, m, payloadPtr, d.Payload, payloadMaxLen)
				return packAwaitSignalsResult(written, payloadWritten, false, 0)
			}
		}
	}

	// Record await and suspend.
	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeAwaitSignals,
		SignalNames: signalNames,
		TimeoutMs:   timeoutMs,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_signals(%s, %dms)", signalNames, timeoutMs),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packAwaitSignalsResult(0, 0, true, 0)
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
func (s *execSession) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	// Fire-and-forget: record the signal event.
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return 0
			}
		}
		s.exitReplay()
	}

	// A fresh signal_workflow is new work: it delivers a signal to another
	// workflow, which can wake it and start a run. A defer segment exists to run
	// a terminated workflow's cleanup, not to start something on its behalf.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	// Check signal authorization before delivering.
	if s.engine.requireSignalAuth && s.engine.signalAuthCheck != nil {
		if err := s.engine.signalAuthCheck(ctx, targetRunID, s.defName); err != nil {
			s.engine.log().ErrorContext(ctx, "signal_auth failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return errSignalAuthRequiredInt
		}
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeSignalReceived,
		SignalName:    signalName,
		SignalPayload: payload,
		RunID:         targetRunID,
	}
	s.recordEvent(rec)

	// Deliver to target via signal store if available.
	if s.engine.signalStore != nil {
		if err := s.engine.signalStore.DeliverSignal(ctx, targetRunID, signalName, payload); err != nil {
			s.engine.log().ErrorContext(ctx, "deliver_signal failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}

	return 0
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
