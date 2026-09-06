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
func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	if s.engine.signalStore != nil {
		d, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, signalName)
		if err == nil && found {

			written, _ := s.writeResult(ctx, m, payloadPtr, d.Payload, payloadMaxLen)
			flags := uint32(0x0100) // found=true
			return int64(uint64(written)<<32 | uint64(flags))
		}
	}
	return 0 // not found
}

func (s *execSession) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}

				written, _ := s.writeResult(ctx, m, responsePtr, rec.SignalPayload, responseMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
	}

	// A fresh send_signal_and_wait is new work twice over: it delivers a signal
	// to another workflow and then blocks this one waiting for a reply that a
	// terminated workflow can never receive.
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

	// Fresh execution: check if target has responded via signal store.
	if s.engine.signalStore != nil {
		d, found, err := s.engine.signalStore.PollSignal(ctx, targetRunID, signalName)
		if err == nil && found {
			rec := EventRecord{
				Step:          s.stepCount,
				EventType:     EventTypeSignalReceived,
				SignalName:    signalName,
				SignalPayload: d.Payload,
			}
			s.recordEvent(rec)
			// Deliberately NOT consumed, unlike the two await paths above.
			//
			// This poll reads targetRunID's queue -- the workflow this one is
			// signalling -- and what it is looking for there is a reply. But
			// SendSignalAndWait never calls DeliverSignal, so it does not
			// actually send the signal it waits on, and until that is settled
			// it is not clear whose delivery this read is finding. Consuming
			// on a path whose semantics are unresolved would delete a row on
			// a guess. See IMPROVEMENT-PLAN 3.220.

			written, _ := s.writeResult(ctx, m, responsePtr, d.Payload, responseMaxLen)
			return packSimpleResult(0, written)
		}
	}

	// No response yet — record event and suspend.
	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeAwaitSignals,
		SignalNames: signalName,
		TimeoutMs:   timeoutMs,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("send_signal_and_wait(%s, %s)", targetRunID, signalName),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packSimpleResult(1, 0)
}

func (s *execSession) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	// Record the reply event for replay fidelity.
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

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeSignalReceived,
		SignalName:    correlationID,
		SignalPayload: response,
	}
	s.recordEvent(rec)

	return 0
}

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
