package engine

import (
	"context"
	"encoding/json"
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
				// Guarded on exhaustion because reaching here with history
				// REMAINING means the original execution moved past this
				// await without receiving anything -- a timeout. Replay is
				// not over in that case and must not be ended; that path is
				// left exactly as it was. It has its own defect, filed as
				// cleat#947 -- the poll hands the guest a signal the
				// original run never saw, consumes it, and writes a record
				// whose Step collides with the event already at that step.
				if s.stepCount >= len(s.history) {
					s.exitReplay()
				}
				if s.engine.signalStore != nil {
					// SignalNames is a JSON array like ["agent_result"].
					var names []string
					if err := json.Unmarshal([]byte(rec.SignalNames), &names); err != nil {
						names = splitSignalNames(rec.SignalNames)
					}
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
				// No signal found. This is a replay of a wait that has not
				// resolved. Should not happen (we only wake when signal
				// arrives), but handle gracefully.
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

	// Fresh execution: check signal store first.
	if s.engine.signalStore != nil {
		names := splitSignalNames(signalNames)
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
