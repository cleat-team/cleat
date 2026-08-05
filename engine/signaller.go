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
				_, _ = s.writeResult(ctx, m, payloadPtr, rec.SignalPayload, payloadMaxLen)
				return packAwaitSignalsResult(written, uint32(len(rec.SignalPayload)), false, 0)
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
						_, _ = s.writeResult(ctx, m, payloadPtr, nextRec.SignalPayload, payloadMaxLen)
						return packAwaitSignalsResult(written, uint32(len(nextRec.SignalPayload)), false, 0)
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
						payload, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, name)
						if err == nil && found {
							// Record the signal_received event so
							// subsequent replays find it in history.
							sigRec := EventRecord{
								Step:          s.stepCount,
								EventType:     EventTypeSignalReceived,
								SignalName:    name,
								SignalPayload: payload,
							}
							s.recordEvent(sigRec)
							written, _ := s.writeResult(ctx, m, sigNamePtr, name, sigNameMaxLen)
							_, _ = s.writeResult(ctx, m, payloadPtr, payload, payloadMaxLen)
							return packAwaitSignalsResult(written, uint32(len(payload)), false, 0)
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

	// Fresh execution: check signal store first.
	if s.engine.signalStore != nil {
		names := splitSignalNames(signalNames)
		for _, name := range names {
			payload, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, name)
			if err == nil && found {
				rec := EventRecord{
					Step:          s.stepCount,
					EventType:     EventTypeSignalReceived,
					SignalName:    name,
					SignalPayload: payload,
				}
				s.recordEvent(rec)

				written, _ := s.writeResult(ctx, m, sigNamePtr, name, sigNameMaxLen)
				_, _ = s.writeResult(ctx, m, payloadPtr, payload, payloadMaxLen)
				return packAwaitSignalsResult(written, uint32(len(payload)), false, 0)
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

			_, _ = s.writeResult(ctx, m, reasonPtr, reason, reasonMaxLen)
			return int64(uint64(len(reason))<<32 | 1) // cancelled=true
		}
	}
	return 0
}

func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	if s.engine.signalStore != nil {
		payload, found, err := s.engine.signalStore.PollSignal(ctx, s.engine.workflowID, signalName)
		if err == nil && found {

			written, _ := s.writeResult(ctx, m, payloadPtr, payload, payloadMaxLen)
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

	// Check signal authorization before delivering.
	if s.engine.requireSignalAuth && s.engine.signalAuthCheck != nil {
		if err := s.engine.signalAuthCheck(ctx, targetRunID, s.defName); err != nil {
			s.engine.log().ErrorContext(ctx, "signal_auth failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return errSignalAuthRequiredInt
		}
	}

	// Fresh execution: check if target has responded via signal store.
	if s.engine.signalStore != nil {
		payload, found, err := s.engine.signalStore.PollSignal(ctx, targetRunID, signalName)
		if err == nil && found {
			rec := EventRecord{
				Step:          s.stepCount,
				EventType:     EventTypeSignalReceived,
				SignalName:    signalName,
				SignalPayload: payload,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, responsePtr, payload, responseMaxLen)
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
