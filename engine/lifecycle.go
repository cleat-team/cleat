package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// suspendedForContinueAsNew reports whether this execution is suspending because the workflow continued as new (the guest's ContinueAsNew, its
// versioned form, or the event-cap auto-continuation: all set the same reason). That suspension is the END of the run, not a wait: the workflow
// is exiting, so its registered defers belong to this execution and must run, exactly as they do for a Go guest and for a guest that returns
// after continuing as new. A component guest that raises SuspendSentinel after continue_as_new does not drain on its own (cleat#4197).
func (s *execSession) suspendedForContinueAsNew() bool {
	return s.suspendErr != nil && s.suspendErr.Reason == "continue_as_new"
}

func (s *execSession) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				s.suspendErr = &SuspendError{
					Reason:   "continue_as_new",
					NewInput: rec.NewInput,
				}
				return 0
			}
			return s.replayDivergence(ctx, m, EventTypeContinueAsNew, 0, 0)
		}
		s.exitReplay()
	}
	// New work, so a defer segment refuses it (cleat#4159). It used to record a continue_as_new event in the history of a workflow
	// being terminated, and a guest that ignored the stop went on into its own defer drain, where the cleanup's call was refused
	// and the whole defer table consumed.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeContinueAsNew,
		NewInput:  newInputJSON,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:   "continue_as_new",
		NewInput: newInputJSON,
	}
	return 0
}

// ContinueAsNewWithVersion restarts the workflow with new input and optionally
// a new version. If newVersion is 0, uses the current version (same as ContinueAsNew).

func (s *execSession) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				s.suspendErr = &SuspendError{
					Reason:     "continue_as_new",
					NewInput:   rec.NewInput,
					NewVersion: rec.NewVersion,
				}
				return 0
			}
			return s.replayDivergence(ctx, m, EventTypeContinueAsNew, 0, 0)
		}
		s.exitReplay()
	}
	// New work, so a defer segment refuses it (cleat#4159). It used to record a continue_as_new event in the history of a workflow
	// being terminated, and a guest that ignored the stop went on into its own defer drain, where the cleanup's call was refused
	// and the whole defer table consumed.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeContinueAsNew,
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:     "continue_as_new",
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	return 0
}

func (s *execSession) Version(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.Version())
	}
	return 1
}

func (s *execSession) MinVersion(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.MinVersion())
	}
	return 1
}

func (s *execSession) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	s.mu.Lock()
	if s.queryState == nil {
		s.queryState = make(map[string]string)
	}
	s.queryState[key] = value
	s.mu.Unlock()
	return 0
}

func (s *execSession) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateHandler {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return 0
			}
			return s.replayDivergence(ctx, m, EventTypeUpdateHandler, 0, 0)
		}
		s.exitReplay()
	}

	// Fresh execution: record the handler registration event.
	rec := EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypeUpdateHandler,
		UpdateHandlerName: name,
	}
	s.recordEvent(rec)
	return 0
}

// exitReplay transitions from replay to forward execution.
//
// It used to also arm a replayJustEnded flag that DurableSleep consumed to
// detect resume-from-sleep. That flag is gone: sleep now decides from elapsed
// time, which does not require another call to have crossed the frontier
// first. See DurableSleep and IMPROVEMENT-PLAN 3.67.
func (s *execSession) exitReplay() {
	s.isReplay = false
}

// recordEvent timestamps a fresh event, advances the session clock,
// and appends it to the history. It must only be called during fresh
// execution (not replay).

// eventPersistence says what happened to an event's durable write, which is
// three outcomes and not two. cleat#1572.
//
// The distinction that matters is NOT ATTEMPTED versus ATTEMPTED AND FAILED.
// Both leave the row absent, and they mean opposite things:
//
//   - Not attempted is a configuration. This engine has no database, or the
//     operator set --no-per-step-flush and the events land at segment end.
//     Nothing is wrong and nothing is lost.
//   - Failed is a refusal. The commonest cause is ErrFenceLost: another worker
//     has taken this run, so THIS worker's events are not going to be in its
//     history at all. Anything shown to a user off the back of one is output
//     the system does not believe happened.
type eventPersistence int

const (
	// eventNotAttempted: no database, or per-step flush is off.
	eventNotAttempted eventPersistence = iota
	// eventPersisted: the row is in the database now.
	eventPersisted
	// eventFlushFailed: the write was attempted and refused or errored.
	eventFlushFailed
)

// eventWriteRetryAfter is how long a run that stopped after a failed event write waits before it is claimed again.
const eventWriteRetryAfter = 5 * time.Second

// stopAfterFailedEventWrite is what a failed write of a recorded event does to the session (cleat#4160). The guest used to carry on: the
// call's effect had happened, its event was not durable, and the next event was written at the next step, so the run's record had a hole
// (measured: two calls executed, one event written, no suspension). A worker lost before the segment-end write then replays the persisted
// prefix, reaches the hole and re-executes everything after it.
//
// ErrFenceLost is NOT handled here: another worker owns the run and the fence already deals with it. A quota refusal is answered with the
// event cap's continue-as-new, because a suspend would be refused again for ever. Anything else stops the session at its next fresh host call
// (stopBeforeNewWork) and suspends the run; the segment-end write then persists the event if the database is back, and otherwise the run is
// reclaimed and replays the persisted prefix, so one step runs again rather than everything after it.
func (s *execSession) stopAfterFailedEventWrite(rec EventRecord, err error) {
	if err == nil || s.eventWriteFailure != nil || s.handlingEventWriteFailure {
		return
	}
	s.handlingEventWriteFailure = true
	defer func() { s.handlingEventWriteFailure = false }()
	reason := "error"
	if errors.Is(err, ErrEventQuotaExceeded) {
		reason = "quota"
	}
	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordEventWriteFailure(context.Background(), reason)
	}
	// Named for an operator to alert on: a run that keeps landing here is being suspended and reclaimed by a store that will not take its events.
	s.engine.log().WarnContext(context.Background(), "recorded event could not be written; stopping the run at its next host call",
		"workflow_id", s.workflowID, "tenant_id", s.tenantID, "step", rec.Step, "event_type", rec.EventType, "reason", reason, "error", err)
	if errors.Is(err, ErrEventQuotaExceeded) {
		// The continuation is the REMEDY for a refused quota, and ContinueAsNew refuses while the session is stopping (a defer segment, a
		// shutdown, #4159) and now also after a failed write. So it runs BEFORE the failure is recorded on the session: with the flag set
		// first it refused itself, set no suspension, and left the run going on top of the hole (found in review of the first version, on a
		// merge with #4161). Its own event write fails on the quota too; the latch above stops that re-entering here.
		if !s.autoContinueAsNewTriggered {
			s.triggerEventCapContinueAsNew(context.Background(), nil)
		}
		s.eventWriteFailure = err
		return
	}
	s.eventWriteFailure = err
	if s.suspendErr == nil {
		s.suspendErr = &SuspendError{
			Reason: fmt.Sprintf("event write failed at step %d (%s): %v", rec.Step, rec.EventType, err),
			Until:  time.Now().Add(eventWriteRetryAfter),
		}
	}
}

// recordEvent appends an event to the session history and persists it.
//
// It returns what became of the durable write. EXISTING CALLERS IGNORE IT and
// still compile, which is deliberate: the return exists for the one caller
// that must not show a user a token the database refused, and adding it should
// not touch the several dozen sites that have no such question.
func (s *execSession) recordEvent(rec EventRecord) eventPersistence {
	if rec.TimestampMs == 0 {
		rec.TimestampMs = time.Now().UnixMilli()
	}
	// Stamped here rather than at each call site, because "was the guest
	// draining its defer table when this happened" is a property of the
	// session at the moment of recording, and there are too many call sites
	// for any of them to be the place that remembers. cleat#1155.
	if s.inDeferPhase {
		rec.InDeferPhase = true
	}
	// Host-observed, unlike the line above: this engine IS a defer segment, so everything it records fresh was written by
	// the segment. cleat#4079.
	if s.engine != nil && s.engine.deferPhase {
		rec.InDeferSegment = true
	}
	// The durable clock must not go backwards (cleat#944).
	//
	// Two clock domains feed Now(). Before any event is recorded it is the
	// seed -- the workflow row's created_at, which is the DATABASE's clock
	// (see Engine.seedNowMs, and note why created_at has to stay the seed: it
	// is the only value identical on the original run and the replay). The
	// first recorded event's timestamp is the WORKER's time.Now(). Nothing
	// reconciled them, so the step between them was the offset between two
	// machines' clocks, in whichever direction they happened to differ.
	//
	// Measured on the samples-go port: PostgreSQL 6 of 8 runs backwards, worst
	// -26ms; MySQL 4 of 5, worst -118ms. Both databases were containers against
	// a host worker. The magnitude tracks the DEPLOYMENT, not the dialect --
	// two clocks on one laptop are close, a worker and a database in different
	// availability zones have no such bound.
	//
	// A workflow computing Now().Sub(start) across its first durable call got a
	// negative duration.
	//
	// CLAMPING HERE RATHER THAN AT THE READ IS WHAT KEEPS REPLAY EXACT, and the
	// reason is sharper than "history would hold a different number".
	//
	// A read-side clamp would make the guest see a value that was never
	// recorded. The original run and the replay would then agree only by both
	// applying the same clamp to the same stale input -- which holds until
	// someone changes the clamp, and nothing would fail at the moment they did.
	// Writing the adjusted value means the recorded number IS the number: it is
	// what the checksum covers, replay sets s.nowMs straight from it
	// (engine/replayer.go), and there is nothing left to recompute or to keep
	// in agreement.
	//
	// (That framing is rcownie-ef's, from the review of this change.)
	//
	// This is a floor, not a rewrite: once the worker clock passes the seed --
	// which it does within the offset, tens of milliseconds -- the branch stops
	// firing and every later timestamp is the worker's own. DurableSleep is
	// unaffected either way, because it sets TimestampMs to anchor+duration,
	// which is already >= s.nowMs.
	if rec.TimestampMs < s.nowMs {
		rec.TimestampMs = s.nowMs
	}
	s.nowMs = rec.TimestampMs
	s.history = append(s.history, rec)
	s.stepCount++
	atomic.AddInt64(&freshStepCount, 1)

	// Persist immediately so events survive worker crashes.
	outcome := eventNotAttempted
	if s.engine.db != nil && !s.isReplay {
		checksum := computeEventChecksum(rec, s.lastChecksum)
		flushed := false
		af := s.engine.getAdaptiveFlusher()
		if af != nil {
			done, useBatch := af.Flush(context.Background(), s.workflowID, rec, checksum, s.engine.workerID, s.engine.generation)
			if useBatch {
				// A blocking receive. It was a select with one case and no
				// default, which is the same thing spelled in a way that
				// suggests a second case was once intended or is coming.
				if err := <-done; err != nil {
					if errors.Is(err, ErrFenceLost) {
						// Expected and normal under reaping (B4): this
						// worker's claim on the workflow was lost, so the
						// event was not written. Not logged as an error --
						// see engine/flush.go's flushEvent doc for why the
						// session is not aborted here either.
						s.engine.log().DebugContext(context.Background(), "adaptive flush: fence lost, workflow reassigned to another worker", "workflow_id", s.workflowID, "step", rec.Step)
					} else {
						s.engine.log().ErrorContext(context.Background(), "adaptive flush failed", "workflow_id", s.workflowID, "step", rec.Step, "error", err)
						s.stopAfterFailedEventWrite(rec, err)
					}
					outcome = eventFlushFailed
				} else {
					s.lastChecksum = checksum
					outcome = eventPersisted
				}
				flushed = true
			}
		}
		if !flushed {
			// Direct flush (low-rate mode or batch/adaptive flushers disabled)
			//
			// RETRIED, which it was not. Of the six routes to
			// eventFlushFailed this is the one a low-rate workflow takes on
			// every step, and it made exactly one attempt: a database that
			// dropped a connection lost the event outright, while the same
			// event at a higher step rate would have been retried by the batch
			// path. The asymmetry was not a decision, it was where the retry
			// happened to be written. cleat#1717.
			//
			// RETRYING IS SAFE HERE BECAUSE A RE-INSERT IS NOT A DOUBLE WRITE.
			// insertEventSQL is ON CONFLICT (tenant_id, workflow_id, step) DO UPDATE
			// with a WHERE that cannot fire (see its doc), so an attempt that
			// committed and lost its acknowledgement replays as zero rows
			// affected -- and afterFencedInsert answers zero rows by asking
			// Heartbeat whether the fence still holds, which it does, so the
			// retry returns nil rather than mistaking its own earlier success
			// for a lost fence. TestARetryOfAnAlreadyCommittedFlushIsNotAFenceLoss
			// measures that rather than trusting this paragraph.
			// With per-step flush OFF the call below writes nothing and returns nil. Keep the event and the checksum it chains
			// from, so a run that FAILS can write what it recorded (flushUnflushedEvents); one that completes or suspends
			// hands the same events to the worker's finalization instead and this slice is never read.
			if s.engine.noPerStepFlush {
				s.unflushed = append(s.unflushed, unflushedEvent{rec: rec, prev: s.lastChecksum})
			}
			flushErr := retryFlushUntilDeadline(context.Background(), s.engine.flushRetryWindow, "direct flush", func() error {
				return s.engine.flushEvent(context.Background(), s.workflowID, rec, s.lastChecksum)
			})
			if flushErr != nil {
				if errors.Is(flushErr, ErrFenceLost) {
					s.engine.log().DebugContext(context.Background(), "flushEvent: fence lost, workflow reassigned to another worker", "workflow_id", s.workflowID, "step", rec.Step)
				} else {
					s.engine.log().ErrorContext(context.Background(), "recordEvent flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "event_type", rec.EventType, "error", flushErr)
					s.stopAfterFailedEventWrite(rec, flushErr)
				}
				outcome = eventFlushFailed
			} else {
				s.lastChecksum = checksum
				// flushEvent returns nil WITHOUT WRITING when per-step flush
				// is disabled, so a nil error is not on its own evidence that
				// the row exists.
				if !s.engine.noPerStepFlush {
					outcome = eventPersisted
				}
			}
		}
	}
	return outcome
}

func (s *execSession) Now(ctx context.Context) int64 {
	// The virtual clock is the LATER of two deterministic anchors, never just
	// the recorded one.
	//
	// During replay, the last consumed event's timestamp reproduces what the
	// original execution saw. But a sleep records no event and advances s.nowMs
	// to anchor+duration (see DurableSleep), and stepCount does not move -- so
	// reading only the history timestamp handed the guest a PRE-sleep instant
	// after a sleep had completed. Measured: 163ms of apparent elapsed time
	// across a 3000ms sleep, until the next event happened to be recorded and
	// re-anchored the clock.
	//
	// Both values are deterministic, so taking the later of them is too:
	// history timestamps are recorded, and s.nowMs is anchor+duration where the
	// anchor is itself one of these. Replay recomputes the same sleeps from the
	// same anchors and arrives at the same number.
	//
	// max, not assignment, for the reason DurableSleep gives for its own max:
	// the clock must never run backwards, and either source can be the larger
	// one depending on where the workflow is.
	now := s.nowMs
	if s.stepCount > 0 && s.stepCount <= len(s.history) {
		if ts := s.history[s.stepCount-1].TimestampMs; ts > now {
			now = ts
		}
	}
	return now
}

func (s *execSession) Random(ctx context.Context) int64 {
	// Deterministic random: seeded from workflow ID and step count.
	// On replay, stepCount is the same for each call, so Random()
	// always returns the same sequence.
	data := fmt.Sprintf("%s:%d:%d", s.workflowID, s.stepCount, s.randomSeq)
	s.randomSeq++
	hash := sha256.Sum256([]byte(data))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (s *execSession) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {

	wfID := s.workflowID
	if wfID == "" {
		wfID = "unknown"
	}
	data := wfID + ":" + seed
	hash := sha256.Sum256([]byte(data))
	// Format as UUIDv5-like value (first 16 bytes of SHA-256, version bits set).
	hash[6] = (hash[6] & 0x0f) | 0x50 // Version 5
	hash[8] = (hash[8] & 0x3f) | 0x80 // Variant 1
	uuidStr := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])

	written, writtenEC := s.writeOut(ctx, m, uuidPtr, uuidStr, uuidMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	return s.step(ctx, m, &sideEffectStep, stepArgs{s1: computedResult, outPtr: respPtr, outMaxLen: respMaxLen})
}

// sideEffectStep is SideEffect as a step (cleat#4152, G1a).
//
// A fresh side_effect is new work: it records a non-deterministic value into the history of a workflow that has already terminated, and any
// later replay would take that value as authoritative, so the dispatcher refuses it in a segment's body on BOTH arrival paths (a defer segment
// over a run that has history starts in replay, so its body's first new call arrives at the replay frontier; cleat#4143, measured on a real
// worker: a terminate's segment made the body's next call and never ran the registered cleanup).
//
// THE RECORDED VALUE WINS, WHATEVER THE GUEST COMPUTED. cleat#4113 (owner decision: side_effect FREEZES a value).
//
// This used to compare the value the guest passed with the recorded one and fail the replay on a difference, calling it "a non-determinism
// bug". But a side effect exists to hold a value that is NOT reproducible (a uuid, a clock read, an API answer), so on replay the two differ
// by construction and the call that was meant to freeze the value was the one that failed the run. The migration guide's own
// `uuid.NewString()` example failed its first replay.
//
// A guest built before SideEffectRecorded still computes the value on every replay, because it cannot know better, so a side effect inside its
// function still re-executes; but it no longer fails, and it gets the value the history holds. SDKs that ask SideEffectRecorded first do not
// run the function at all.
//
// WHAT THE MOVE CHANGES, and the characterization says so (a_side_effect_and_defer_single_steps_behave_as_recorded_before_the_dispatcher_test.go):
// on a divergence the old replay consumed the record before checking its type, so the record that belonged to another call was lost and every
// later step read the history one place late; step consumes nothing on a divergence (cleat#4157), as every other call does. The divergence
// message is now the shared one.
// decodeRecordedSideEffect hands the guest the value the history holds, whatever it computed: the one thing SideEffect and its lookup share on replay.
func decodeRecordedSideEffect(c *stepCtx, rec *EventRecord) int64 {
	written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, rec.SideEffectResult, c.args.outMaxLen)
	return packSimpleResult(writtenEC, written)
}

var sideEffectStep = stepSpec{
	Name:       "side_effect",
	Effect:     effectEffect,
	Records:    recordsOne,
	Want:       EventTypeSideEffect,
	NoIdentity: "the value is the event: a side effect records the non-deterministic output itself, and the guest's own computation of it differs between a run and its replay by construction",
	Decode:     decodeRecordedSideEffect,
	Fresh: func(c *stepCtx) int64 {
		c.Record(EventRecord{
			Step:             c.s.stepCount,
			EventType:        EventTypeSideEffect,
			SideEffectResult: c.args.s1,
		})
		written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, c.args.s1, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

// sideEffectNotRecorded is the status SideEffectRecorded returns when the history holds no value for this step. It is not an error code: a
// guest that gets it computes the value and calls SideEffect, which records it.
const sideEffectNotRecorded byte = 2

// SideEffectRecorded answers one question for a guest about to run a side effect: "does the history already hold the value for this step?"
// (cleat#4113). If it does, the step is consumed and the recorded value is returned, so the guest does NOT run its function. If it does not,
// nothing is recorded and nothing changes: the guest computes the value and calls SideEffect, which records it.
//
// Status, in the low byte of a simple-pack word: 0 = a recorded value, written to the output buffer; 1 = an error, with its message; 2 =
// sideEffectNotRecorded. The stop sentinel is returned for a lookup that would be NEW work in a workflow the host has stopped, whether the session has left replay
// or is at the replay frontier: the SideEffect that follows would be refused anyway, and refusing here means the guest's function, which may call an API, never runs.
//
// The three cases, which the tests pin one by one:
//   - replaying, and the record at this step is a side_effect: consume it, return its value;
//   - replaying, and the record is some other event: a replay divergence, reported the way every other call reports it, and the step is
//     NOT consumed (cleat#1507 -- check before consuming, so the record that belongs to another operation is still there);
//   - fresh, or replaying past the end of history: "not recorded", with no state change, or the stop sentinel when the host has stopped
//     the workflow. The switch from replay to fresh stays in SideEffect, where it has always been.
func (s *execSession) SideEffectRecorded(ctx context.Context, m api.Module, respPtr, respMaxLen uint32) int64 {
	return s.step(ctx, m, &sideEffectRecordedStep, stepArgs{outPtr: respPtr, outMaxLen: respMaxLen})
}

// sideEffectRecordedStep is SideEffectRecorded as a step: the Lookup kind of the design (cleat#4152, G1a).
//
// It is the step BEFORE new work: an SDK that asks first runs the guest's function, which may call an API, between this call and the SideEffect
// that records its value. So a fresh lookup is refused when the host has stopped the workflow, and so is one at the replay frontier (a segment
// over a run with history starts in replay, so the body's first new side effect arrives at that boundary; cleat#4143, measured through the
// real C# SDK: a defer body saw the function run once in a segment over a history cut at the frontier, and not at all once the lookup
// refused). A RECORDED value is read, not refused: reading history is not new work.
//
// StaysInReplayAtFrontier: at the frontier the lookup answers "not recorded" and does NOT leave replay; the SideEffect that follows makes the
// switch, as it always has.
//
// Changes from the old code, named by the characterization: the divergence message is the shared one; and a debugger's ReplayQuit now answers
// 0 where the lookup used to answer the stop sentinel (the dispatcher answers 0 for every spec; debugger-only).
var sideEffectRecordedStep = stepSpec{
	Name:                    "side_effect_recorded",
	Effect:                  effectEffect,
	Records:                 recordsNone,
	Want:                    EventTypeSideEffect,
	StaysInReplayAtFrontier: true,
	NoIdentity:              "the value is the event: the lookup reads the same record SideEffect writes, and has no argument that could identify it",
	Decode:                  decodeRecordedSideEffect,
	Fresh:                   func(*stepCtx) int64 { return packSimpleResult(sideEffectNotRecorded) },
}

func (s *execSession) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	id := s.workflowID
	if id == "" {
		id = "unknown"
	}
	written, writtenEC := s.writeOut(ctx, m, idPtr, id, idMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	runID := s.execRunID
	if runID == "" {
		runID = "unknown"
	}
	written, writtenEC := s.writeOut(ctx, m, idPtr, runID, idMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	// Retained as a harmless ABI-compatibility no-op only -- see the
	// RegisterQueryHandler doc comment on the HostHandler interface in
	// imports.go. Nothing reads s.queryHandlers back out to dispatch a
	// query to it; "invoked out-of-band by the worker" was never true.
	s.queryHandlers = append(s.queryHandlers, name)
	return 0
}

// ---- Stream R host functions ----

// IncrState atomically increments a numeric state value.  It is NOT safe for
// concurrent access from multiple WASM modules.  The engine serialises all
// host calls within a single workflow execution, so this is never called
// concurrently in practice — speculative parallelism MUST NOT be introduced
// without adding synchronisation to IncrState.

func (s *execSession) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {
	return s.step(ctx, m, &fetchStep, stepArgs{s1: method, s2: url, s3: headersJSON, s4: body, outPtr: responsePtr, outMaxLen: responseMaxLen})
}

// fetchStep is Fetch as a step (cleat#4152, G1b). Past the frontier in a defer segment an outbound HTTP request is new work, and the most
// externally visible kind there is -- it leaves a side effect on someone else's server that no amount of unwinding takes back (IMPROVEMENT-PLAN
// 3.84 guarded six fresh paths and this was the seventh it did not list) -- so the dispatcher refuses it, with the same universal sentinel.
//
// TWO BEHAVIOUR CHANGES, both the cleat#4152 defect and both named in the PR. (1) The old replay CONSUMED the record before comparing it, so a
// Fetch whose method, url or body differed from the recorded one lost the record and every later step read the history one place late; it
// now consumes nothing. (2) The divergence text is the shared one, whose why-string names the method and url and says whether the body
// differs; the old text quoted both bodies and the recorded response. The identity is what the old code compared (method, url, body); the
// headers are NOT compared, as before.
var fetchStep = stepSpec{
	Name:    "fetch",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeFetch,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.FetchMethod != a.s1 || rec.FetchURL != a.s2 {
			return fmt.Sprintf("%s %s, recorded %s %s", a.s1, a.s2, rec.FetchMethod, rec.FetchURL)
		}
		if rec.FetchBody != a.s4 {
			return fmt.Sprintf("%s %s with a different body: %q, recorded %q", a.s1, a.s2, truncateWithHash(a.s4, maxPayloadLen), truncateWithHash(rec.FetchBody, maxPayloadLen))
		}
		return ""
	},
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			written, _ := c.s.writeResult(c.ctx, c.m, c.args.outPtr, rec.Err, c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
		written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, rec.FetchResponse, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, m := c.s, c.ctx, c.m
		method, url, headersJSON, body := c.args.s1, c.args.s2, c.args.s3, c.args.s4
		responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen

		var response string
		var fetchErr error
		if s.engine.fetcher != nil {
			// Scoped to this execution's tenant, as callService does for a durable call: the worker's fetcher answers to the tenant's egress
			// allowlist, and without a tenant in context that list denies everything.
			response, fetchErr = s.engine.fetcher.Fetch(s.tenantScopedContext(ctx), method, url, headersJSON, body)
		} else {
			// Not a failure of the guest's request: this engine was built without a Fetcher. On a cleat-worker that is the default, and the
			// operator turns it on (IMPROVEMENT-PLAN.d 3.317, cleat#3377); in an embedding host it is engine.WithFetcher. Name both, so the
			// reader is sent to whichever applies instead of to a flag that does not exist where they are.
			fetchErr = fmt.Errorf(
				"cleat_fetch is disabled: this engine has no Fetcher. A cleat-worker "+
					"serves cleat_fetch only when started with --enable-workflow-fetch "+
					"(off by default; docs/operations/egress-policy.md); an embedding "+
					"host supplies engine.WithFetcher(...). Workflow %s attempted %s %s",
				s.engine.workflowID, method, url)
		}

		rec := EventRecord{
			Step:          s.stepCount,
			EventType:     EventTypeFetch,
			FetchMethod:   method,
			FetchURL:      url,
			FetchHeaders:  headersJSON,
			FetchBody:     body,
			FetchResponse: response,
		}
		if fetchErr != nil {
			rec.Err = fetchErr.Error()
		}
		c.Record(rec)

		if fetchErr != nil {
			written, _ := s.writeResult(ctx, m, responsePtr, fetchErr.Error(), responseMaxLen)
			return packSimpleResult(1, written)
		}

		written, writtenEC := s.writeOut(ctx, m, responsePtr, response, responseMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

// JsonParse validates and canonicalises input using the host's encoding/json.
//
// input arrives already decoded, and output goes through writeResult. Both
// matter: this handler used to take raw (ptr, len) and call m.Memory()
// directly, which made it the only host function reading its own input out of
// guest memory. The wasmtime backend passes a nil api.Module by design -- the
// memory is handed over in the context instead (see writeResult) -- so
// m.Memory() was a nil dereference and every cleat_json_parse call from a
// guest crashed the execution on the primary backend. See
// IMPROVEMENT-PLAN.md 2.14.
func (s *execSession) JsonParse(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := s.writeResult(ctx, m, outPtr, string(normalized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}

// JsonStringify re-serialises input via the host's encoding/json. See
// JsonParse for why input is a string rather than a (ptr, len) pair.
func (s *execSession) JsonStringify(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	serialized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := s.writeResult(ctx, m, outPtr, string(serialized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}
