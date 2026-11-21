package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
	"go.opentelemetry.io/otel/attribute"

	"github.com/cleat-team/cleat/internal/telemetry"
)

// Atomic counters for throughput computation. Sampled by the worker outside of
// the Prometheus registry to avoid prometheus.Collect overhead.
var (
	replayStepCount int64
	freshStepCount  int64
	freshCallCount  int64
)

// ReplayStepCount returns the total replay step count from the atomic counter.
func ReplayStepCount() int64 { return atomic.LoadInt64(&replayStepCount) }

// FreshStepCount returns the total fresh step count from the atomic counter.
func FreshStepCount() int64 { return atomic.LoadInt64(&freshStepCount) }

// FreshCallCount returns the total fresh DurableCall count from the atomic counter.
func FreshCallCount() int64 { return atomic.LoadInt64(&freshCallCount) }

// stopBeforeNewWork reports whether this call must be refused with
// callSuspendSentinel instead of executed.
//
// True only inside a defer segment (WithDeferPhase), for a call the workflow
// BODY is making past the end of its recorded history. Every host call that can
// start fresh work consults it, not just this one -- plugin calls, child
// workflows and a fresh signal await all reach it (3.84). A defer segment exists
// to run a terminated workflow's cleanup; running its body as well performs the
// side effect the termination was meant to stop, and lets the segment return a
// completion result for a workflow that did not complete. Both were measured --
// IMPROVEMENT-PLAN 3.83.
//
// The defer bodies' own calls must go through, and the host does not need the
// guest to tell it which is which: the host invokes __cleat_run_deferred itself
// (runGuestDefersAfterSuspend), so it brackets that call with inDeferDrain.
// 3.81 assumed a guest-to-host signal was needed here and it is not.
func (s *execSession) stopBeforeNewWork() bool {
	if s.engine == nil {
		return false
	}
	if s.engine.deferPhase && !s.inDeferDrain {
		return true
	}
	// A recorded event that could not be written is a hole in the durable record; no further work goes on top of it (cleat#4160).
	if s.eventWriteFailure != nil {
		return true
	}
	return s.engine.shutdownObserved()
}

// callRefusedAfterSuspend is the message of a call that was refused because this execution has already suspended, for the calls whose answer is an
// ordinary error and not a stop word (cleat#4285).
const callRefusedAfterSuspend = "the workflow has suspended; this call was not run"

// suspendedAlready reports whether this EXECUTION has already suspended: a call answered the guest with a suspension (a child await, a wait, a
// stop), or ContinueAsNew set one, and the guest is still running. A guest is told to stop when that happens and some do not (an SDK returns the
// suspension as an ordinary value, or the guest catches the exception or discards the error: cleat#4285), and every call it makes afterwards is
// recorded AFTER the pending await and runs AGAIN when the workflow resumes. A defer drain is exempt when the HOST runs it (inDeferDrain), and when the
// GUEST runs its own and says so (SetDeferPhase) only if that drain began BEFORE the suspension or the suspension is a continue-as-new (guestDrainIsExempt).
func (s *execSession) suspendedAlready() bool {
	return s.suspendErr != nil && !s.inDeferDrain && !s.guestDrainIsExempt()
}

// guestDrainIsExempt reports whether the guest is draining its own defer table in a way a post-suspension refusal must not stop. Two cases, and the narrowness
// is the point: (1) the drain BEGAN BEFORE the suspension, a cleanup in progress whose body then awaited a child (cleat#3429); (2) the suspension IS the end
// of the run, a continue-as-new, whose registered defers belong to this execution and must run (suspendedForContinueAsNew). A drain that began AFTER a wait
// suspension is neither: it is a wrapper running the defers because the guest swallowed the suspension (Java's catch (Exception), C#'s EntryHost), and its
// calls would be recorded after the pending await and run again on resume, which is cleat#4285. Reported by the reviewer of #4295.
func (s *execSession) guestDrainIsExempt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inDeferPhase && (!s.deferPhaseBeganSuspended || s.suspendedForContinueAsNew())
}

// refuseIfSuspended is the host-side half of cleat#4285: when this execution has already suspended, a call that would start fresh work is refused,
// runs nothing and records nothing, and answers `word`, the word its own SDK decoder recognises (callSuspendSentinel for every stop site, which
// DurableCall's decoders and the other five layouts already test first). It returns the answer and true when it refused. One helper for every call
// off the dispatcher, so that each later migration just moves its call from here to step(), where the same refusal is applied by the spec.
func (s *execSession) refuseIfSuspended(call string, word int64) (int64, bool) {
	return s.refuseIfSuspendedWith(call, func() int64 { return word })
}

// refuseIfSuspendedWith is refuseIfSuspended for a call whose answer is computed (an ordinary error with a message written to the guest's buffer, for a
// call that is not a stop site and so has no stop word its decoders know). It logs a warning each time, so that a guest or an SDK that keeps running
// after a suspension is visible to an operator and not only to a replay that later runs a call twice.
func (s *execSession) refuseIfSuspendedWith(call string, answer func() int64) (int64, bool) {
	if !s.suspendedAlready() {
		return 0, false
	}
	if s.engine != nil {
		s.engine.log().WarnContext(context.Background(), "a host call was made after the workflow had already suspended and was refused: the guest did not stop at the suspension (cleat#4285)",
			"workflow_id", s.workflowID, "call", call, "suspension", s.suspendErr.Reason)
	}
	return answer(), true
}

// setDeferDrain brackets the host's own call to the guest's defer runner. It
// is unexported and reached by interface assertion from the backend, so no
// caller outside the engine can permit new work in a defer segment.
func (s *execSession) setDeferDrain(on bool) { s.inDeferDrain = on }

// callRecordsNothing* name the paths on which a call answers WITHOUT recording the call's event, each by the reason its Fresh gives to RecordsNothing, so callStep
// can list them (RecordsNothingOn) and a Fresh that records nothing for a reason that is not here is reported (cleat#4152, G3). The event cap is NOT one of them:
// it records a continue_as_new event, which is counted through ContinueAsNewAtEventCap.
const (
	callRecordsNothingCancelled = "the workflow was cancelled: the call is not made and the guest is told so"
	callRecordsNothingHeartbeat = "the worker's heartbeat is presumed lost: the call is not started and the guest is told so"
	callRecordsNothingHardStop  = "the worker hard-stopped mid-call: its outcome is unknown, so none is recorded and the next worker replays from before it"
	callRecordsNothingIntent    = "a write-ahead intent that could not be written, completed or honoured: nothing is recorded and the guest is told why"
)

// callStep is cleat_call as a dispatcher step (cleat#4152, G3): one call event, in the durable-call result layout. The refusals a stop or a suspension asks for,
// the replay-to-fresh hand-off at the frontier, the divergence report and the consumption of the record are the dispatcher's; what stays in Fresh is
// everything the call decides for itself (the cancellation poll, a worker presumed lost, the event cap, a hard stop, a write-ahead intent).
var callStep = stepSpec{
	Name:    "call",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeCall,
	// The guest decodes result<string, call-failure>: a divergence is answered in the durable-call layout (ABI.md, "Return packing" of cleat_call).
	// AfterSuspend is left nil on purpose, as for every durable-call-family spec: the refusal after a suspension is the dispatcher's and answers the stop word.
	Result: layoutDurableCall,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.Service != a.s1 || rec.Op != a.s2 {
			return fmt.Sprintf("call %s.%s, recorded %s.%s", a.s1, a.s2, rec.Service, rec.Op)
		}
		return ""
	},
	RecordsNothingOn: []string{callRecordsNothingCancelled, callRecordsNothingHeartbeat, callRecordsNothingHardStop, callRecordsNothingIntent},
	// What the plain call printed on a divergence and docs/troubleshooting.md shows: the two request bodies (TestEngineReplayDivergence pins the labels).
	DivergenceDetail: requestDivergenceDetail,
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		return c.s.answerRecordedCall(c.ctx, c.m, *rec, c.args.outPtr, c.args.outMaxLen)
	},
	Fresh: func(c *stepCtx) int64 { return c.s.freshCall(c) },
}

func init() { registerStepSpec(&callStep) }

// DurableCall is cleat_call. A call made while the session is replaying is a replay step, and it is counted FIRST, before anything is read: the old
// replayCall counted it before it read the record, so a replay that then diverged, quit or reached the frontier and handed off to a fresh call had been counted
// too, and the dispatcher counts none of this for any call that goes through it. a_call_emits_the_same_counters_on_replay_and_fresh_before_the_dispatcher_test.go
// pins the counts and this is what keeps them (cleat#4152, G3).
func (s *execSession) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		if s.engine.Metrics != nil {
			s.engine.Metrics.RecordReplayStep(ctx, s.defName)
		}
		atomic.AddInt64(&replayStepCount, 1)
	}
	return s.step(ctx, m, &callStep, stepArgs{s1: service, s2: operation, s3: requestJSON, outPtr: responsePtr, outMaxLen: responseMaxLen})
}

// requestDivergenceDetail is the detail of a call that carries a request body: the plain call, the retried call and the heartbeat call all printed these two lines
// when they diverged, and docs/troubleshooting.md shows them. The bodies are truncated with a hash, as the old messages truncated them (cleat#4152, G3).
func requestDivergenceDetail(a stepArgs, rec *EventRecord) string {
	return fmt.Sprintf("\n  actual request: %s\n  expected request: %s", truncateWithHash(a.s3, maxPayloadLen), truncateWithHash(rec.Request, maxPayloadLen))
}

// atEventCap reports whether this session has recorded as many durable events as --max-quota-events allows.
func (s *execSession) atEventCap() bool {
	return s.engine.maxEventsPerWorkflow > 0 && s.eventCount >= s.engine.maxEventsPerWorkflow
}

// triggerEventCapContinueAsNew starts the continue-as-new that the event cap asks for. freshCall and the no-wait awaits share it, so
// the cap is one rule with one implementation (cleat#4153): a recorded event that did not pass through here wrote into a quota the
// flush refuses ("event quota exceeded"), retried the write for the whole flush window, and carried on.
func (s *execSession) triggerEventCapContinueAsNew(ctx context.Context, m api.Module) {
	s.autoContinueAsNewTriggered = true
	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordContinueAsNew(ctx, "event_cap")
	}
	s.engine.log().InfoContext(ctx, "auto-ContinueAsNew triggered", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "event_count", s.eventCount, "max", s.engine.maxEventsPerWorkflow)
	s.ContinueAsNew(ctx, m, s.originalInput)
}

// refuseAtEventCap is the cap check for a recorded event that does not go through freshCall (the no-wait awaits). At the cap it
// triggers the continue-as-new once and reports true, and the caller returns the stop the continue-as-new set: every SDK reads
// suspendingAwaitResult as a stop and unwinds. Below the cap it counts the event it is about to record and reports false.
func (s *execSession) refuseAtEventCap(c *stepCtx) bool {
	if s.atEventCap() {
		if !s.autoContinueAsNewTriggered {
			c.ContinueAsNewAtEventCap()
		}
		return true
	}
	s.eventCount++
	return false
}

// freshCall is the call's Fresh: it runs once the dispatcher has decided fresh work is allowed. The refusal for a stop (a defer segment, a shutdown, a refused
// event write) and for a session that has already suspended is the dispatcher's, applied on BOTH ways in (a call that arrives fresh and one that reaches the
// replay frontier), which is what freshCall used to apply itself for the second way after cleat#4143.
func (s *execSession) freshCall(c *stepCtx) int64 {
	ctx, m := c.ctx, c.m
	service, operation, requestJSON := c.args.s1, c.args.s2, c.args.s3
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen
	atomic.AddInt64(&freshCallCount, 1)

	if s.engine.Metrics != nil {
		// Same value, same label, adjacent lines: cleat_calls_total carried no
		// label at all until cleat#1444, so its dashboard panel grouped by
		// workflow_name and drew a single line for every definition.
		s.engine.Metrics.RecordCall(ctx, s.defName)
		s.engine.Metrics.RecordFreshStep(ctx, s.defName)
	}

	// Check cancellation before making the call.
	callCtx := ctx
	if s.engine.hardStopCtx != nil {
		// cleat#2287: ctx is the wasmtime host function's own context, derived
		// from context.Background() and never cancelled, so an in-flight
		// callService would otherwise ride out its own timeout even after the
		// worker hard-stops. Derive a context the blocking call can observe and
		// abort on, without leaking a goroutine past freshCall.
		var cancel context.CancelFunc
		callCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(s.engine.hardStopCtx, cancel)
		defer stop()
	}
	if s.engine.signalStore != nil {
		cancelled, _, err := s.engine.signalStore.PollCancellation(ctx, s.engine.workflowID)
		if err != nil {
			// Failing open is the right default -- a database blip must not
			// abort a workflow that has not been cancelled -- but it was
			// previously silent, so a persistently failing poll made
			// cancellation quietly stop working with nothing to see.
			s.engine.log().WarnContext(ctx, "cancellation poll failed",
				"workflow_id", s.engine.workflowID, "step", s.stepCount, "error", err)
		}
		if err == nil && cancelled {
			// Not retryable: the workflow was cancelled, so repeating the call
			// is the one thing the caller must not do.
			c.RecordsNothing(callRecordsNothingCancelled)
			written, _ := s.writeResult(ctx, m, responsePtr, cancelledCallError, responseMaxLen)
			return packDurableCallResult(int(written), callErrorUnknown, 1)
		}
	}

	// cleat#2008 decision 2: refuse to start new work while the worker's own
	// heartbeat is presumed lost. Checked after cancellation (a cancelled
	// workflow should report cancelled, not unavailable) and before the event
	// cap (which itself starts new work -- a fresh run via ContinueAsNew --
	// that a fenced-out worker has no business kicking off either).
	if s.engine.canStartNewWork != nil && !s.engine.canStartNewWork() {
		c.RecordsNothing(callRecordsNothingHeartbeat)
		written, _ := s.writeResult(ctx, m, responsePtr, heartbeatPresumedLostCallError, responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	// Check event cap: if the number of events has reached the limit, auto-trigger
	// ContinueAsNew to start a fresh run with reset event_count. Events are
	// tracked locally in the session (no DB query per call).
	if s.engine.maxEventsPerWorkflow > 0 && s.eventCount >= s.engine.maxEventsPerWorkflow && !s.autoContinueAsNewTriggered {
		c.ContinueAsNewAtEventCap()
		// Refuse the call rather than closing the module.
		//
		// This branch used to call m.CloseWithExitCode to stop the guest and
		// then hand it an empty SUCCESS, which is two bugs. The wasmtime host
		// functions pass a nil api.Module (wasmtime_hostfuncs.go), so on the
		// only backend a worker runs, m.CloseWithExitCode was a method call on
		// a nil interface -- a nil dereference every time an operator's
		// --max-quota-events cap was reached. It did not surface as a workflow
		// failure: ContinueAsNew above had already set session.suspendErr, so
		// executor.go's "callErr != nil && suspendErr == nil" branch did not
		// take it, and control reached `res.Suspended` with a nil res. That
		// second dereference is in no recover, so the worker PROCESS died, and
		// died again on every worker that picked the workflow up.
		//
		// A refusal needs no module handle and no new ABI. The guest's own
		// error path unwinds it, which drains its defer table on the way out --
		// the same shape an explicit ContinueAsNew already has, where the guest
		// returns normally and its wrapper runs the defers. suspendErr is
		// already set, so the executor still reports a continue_as_new
		// suspension rather than the error the guest returned.
		written, _ := s.writeResult(ctx, m, responsePtr, eventCapCallError, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}
	s.eventCount++

	step := s.stepCount
	callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "call", service, operation)
	defer eventSpan.End()
	callStart := time.Now()

	// An operation declared WriteAheadIntent commits a pending row before the
	// call is dispatched, so a crash mid-call is detectable on replay instead
	// of silently repeating the side effect. It persists the event itself --
	// see freshCallWithIntent for why it must not also flow through
	// recordEvent -- so it returns here rather than falling through.
	if s.engine.callSemantics(service, operation) == WriteAheadIntent {
		resp, intentRec, intentChecksum, recorded, err := s.freshCallWithIntent(callCtx, service, operation, requestJSON, step)
		if recorded {
			c.RecordPersisted(intentRec, intentChecksum)
		}
		if err != nil && !recorded {
			// The intent could not be written, completed or honoured: nothing reached the history. A service error that WAS recorded is an event like any
			// other and is not this path.
			c.RecordsNothing(callRecordsNothingIntent)
		}
		if DebugTiming {
			s.engine.log().InfoContext(ctx, "TIMING: freshCallWithIntent", "workflow_id", s.workflowID, "step", step, "call_ms", time.Since(callStart).Milliseconds())
		}
		if err != nil {
			written, _ := s.writeResult(ctx, m, responsePtr, err.Error(), responseMaxLen)
			return packDurableCallResult(int(written), callFailureCode, 1)
		}
		written, writtenEC := s.writeOut(ctx, m, responsePtr, resp, responseMaxLen)
		return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
	}

	resp, err := s.callService(callCtx, service, operation, requestJSON, step)
	callElapsed := time.Since(callStart)

	if err != nil && s.engine.hardStopObserved() {
		// cleat#2287: the worker hard-stopped mid-call. Suspend, do not fail
		// and do not complete. suspendErr is what makes executor.go's suspend
		// path win over the error even when the guest swallows it and returns
		// normally (the cleat#2285 COMPLETED hazard). No call event is
		// recorded: the call's outcome is unknown, and the next worker replays
		// from before it under the same idempotency key. The error handed to
		// the guest unwinds its error path, draining its defer table.
		s.suspendErr = &SuspendError{Reason: shutdownSuspendReason}
		c.RecordsNothing(callRecordsNothingHardStop)
		written, _ := s.writeResult(ctx, m, responsePtr, shutdownCallError, responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	var callErr string
	if err != nil {
		callErr = err.Error()
	}

	rec := EventRecord{
		Step:      step,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
		Err:       callErr,
	}
	c.Record(rec)

	if DebugTiming {
		s.engine.log().InfoContext(ctx, "TIMING: freshCall", "workflow_id", s.workflowID, "step", rec.Step, "call_ms", callElapsed.Milliseconds())
	}

	if err != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, err.Error(), responseMaxLen)
		return packDurableCallResult(int(written), callFailureCode, 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, resp, responseMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}

// answerRecordedCall answers a call from the record that was consumed for it: a pending intent (the call was dispatched and the outcome was never
// recorded) is resolved or reported ambiguous, a recorded failure is replayed as the failure it was and with the class it had, and anything else is the
// call's recorded response. It is the half of replay that follows the divergence checks, so a call that does its own checking (the retried call, through
// the step dispatcher, cleat#4152 G3) shares it with replayCall and the two cannot drift. It consumes nothing and records nothing of its own beyond what
// the ambiguity handling already wrote.
func (s *execSession) answerRecordedCall(ctx context.Context, m api.Module, rec EventRecord, responsePtr, responseMaxLen uint32) int64 {
	// Detect a pending call intent: the external call was dispatched
	// but the outcome was never persisted.  Return ErrAmbiguous so
	// the caller can check the external service before retrying.
	if rec.isPendingIntent() {
		if s.engine.Metrics != nil {
			s.engine.Metrics.RecordAmbiguousCall(ctx)
		}

		// cleat#2897, decision (c) on cleat#1984: for an operation declared
		// to support idempotency-key replay, try that FIRST -- re-dispatch
		// under the SAME key and let the service's own key table resolve
		// it, which is the default path a declared service gets. Disjoint
		// from ambiguityResolver's configured operations by construction
		// (cleat-worker boot validation), so this never races the lookup
		// path below for the same operation; it is either one or the
		// other, decided by which flag named this service.operation.
		if resp, ok := s.resolveAmbiguityViaKeyReplay(ctx, rec); ok {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx, attribute.String("outcome", "key_replay_resolved"))
			}
			written, writtenEC := s.writeOut(ctx, m, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
		}

		// Ask, before giving up. A resolver that can look the operation up
		// by its idempotency key turns most ambiguities into non-events:
		// the outcome is recorded and replay carries on as though the call
		// had returned normally, which it did -- the crash lost the answer,
		// not the effect. See IMPROVEMENT-PLAN 1.4 phase E.
		switch resp, outcome := s.resolveAmbiguity(ctx, rec); outcome {
		case AmbiguityResolved:
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx, attribute.String("outcome", "resolved"))
			}
			written, writtenEC := s.writeOut(ctx, m, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
		case AmbiguityNotSent:
			// The service confirmed this call never arrived: a definite,
			// retryable failure, not an ambiguity. Same packing as a fresh
			// call failure (callFailureCode), so the guest sees an
			// ordinary Retryable()==true error and its own retry handling
			// takes it from there -- no engine-side re-dispatch needed.
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordAmbiguousCall(ctx, attribute.String("outcome", "not_sent"))
			}
			written, _ := s.writeResult(ctx, m, responsePtr, ambiguityNotSentMessage, responseMaxLen)
			return packDurableCallResult(int(written), callFailureCode, 1)
		}

		// Record the condition structurally before writing the message.
		// The message is for the workflow author; this is for the
		// operator, who needs to find these with a query rather than a
		// substring search. See IMPROVEMENT-PLAN 3.24.
		if s.engine.Metrics != nil {
			s.engine.Metrics.RecordAmbiguousCall(ctx, attribute.String("outcome", "cannot_say"))
		}
		s.recordAmbiguity(rec)

		ambiguousErr := fmt.Sprintf(
			"[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s was dispatched but the response was not recorded before a crash. Check the external service before retrying.",
			rec.Step, rec.Service, rec.Op)
		// Not retryable, and this is the case the old blanket "timeout"
		// got most wrong: the call may well have succeeded. Telling the
		// guest to retry is telling it to risk a duplicate side effect.
		written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
		return packDurableCallResult(int(written), callErrorUnknown, 1)
	}

	if rec.Err != "" {
		// The classification comes off the event now rather than being
		// assumed, so a call the caller marked non-retryable replays as
		// non-retryable. Events written before 2.35 carry no such key and
		// read back false, which is the constant this used to hardcode.
		written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
		return packDurableCallResult(int(written), recordedFailureCode(rec.ErrNonRetryable), 1)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, rec.Response, responseMaxLen)
	return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
}

// retryPolicyFitsBudget reports whether a policy's WORST-CASE total backoff is
// small enough to run on the host, inside one segment.
//
// Worst case, not expected: every attempt fails and every backoff is waited out
// in full. A policy is either always in-segment or always suspending, decided
// before the first attempt, because a policy that switched paths part-way
// through would produce a history whose shape depended on which services
// happened to fail.
//
// This is the arithmetic that used to live in each guest SDK -- Go's
// retryFitsInOneSegment and Rust's retry_fits_in_one_segment, kept equal by a
// test that regex-scraped one language's source from the other's. It is
// transcribed here once, and deleted there; §3.94 step 4 is exactly that move.
//
// A budget of zero means unbounded, matching ClampToCeiling's convention that
// a non-positive limit is "no limit".
func retryPolicyFitsBudget(maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	budget time.Duration) bool {

	if budget <= 0 {
		return true
	}
	total := time.Duration(0)
	interval := guestMsDuration(initialIntervalMs)
	maxInterval := guestMsDuration(maxIntervalMs)

	// maxAttempts attempts means maxAttempts-1 backoffs; the last failure is
	// not followed by a wait.
	for i := int64(1); i < maxAttempts; i++ {
		if maxInterval > 0 && interval > maxInterval {
			interval = maxInterval
		}
		total += interval
		if total > budget {
			return false
		}
		if backoffCoefficient100x > 100 {
			interval = time.Duration(float64(interval) * float64(backoffCoefficient100x) / 100)
		}
	}
	return true
}

// retryStep is the retried call (cleat_call_retry) as a dispatcher step (cleat#4152, G3). It is a MULTI-RECORD step, like the heartbeat call: a run of
// call_attempt_failed events, one per failed attempt that another attempt followed, then the call's outcome, which is a call event or, for a policy
// refused for its length (cleat#4032), a call_retry_refused event. A history that ENDS inside the run of attempts is a worker lost while the policy was
// running, and the policy is finished from the attempts already spent (cleat#1145) and not restarted. The service and operation identify the call, on
// EVERY record of the run: the old replay loop compared them on the attempts only by stopping at the first mismatch, and left the record unread.
var retryStep = stepSpec{
	Name:             "call_with_retry",
	DivergenceDetail: requestDivergenceDetail,
	Effect:           effectEffect,
	Records:          recordsMany,
	Want:             EventTypeCall,
	// The guest decodes result<string, call-failure>: a divergence is answered in the durable-call layout (ABI.md, "Return packing" of cleat_call_retry).
	// AfterSuspend is left nil on purpose, as for every durable-call-family spec: the refusal after a suspension is the dispatcher's own, and answers the stop word.
	Result: layoutDurableCall,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.Service != a.s1 || rec.Op != a.s2 {
			return fmt.Sprintf("call %s.%s, recorded %s.%s", a.s1, a.s2, rec.Service, rec.Op)
		}
		return ""
	},
	Role: func(_ stepArgs, rec *EventRecord) recordRole {
		switch rec.EventType {
		case EventTypeCallAttemptFailed:
			return rolePrefix
		case EventTypeCall, EventTypeCallRetryRefused:
			return roleFinal
		}
		return roleForeign
	},
	DecodeRun: func(c *stepCtx, run []EventRecord) int64 { return c.s.decodeRetryOutcome(c, run[len(run)-1]) },
	Fresh:     func(c *stepCtx) int64 { return c.s.freshCallWithRetry(c) },
}

func init() { registerStepSpec(&retryStep) }

// DurableCallWithRetry is cleat_call_retry. The ONE check that stays here, before the dispatcher, is a policy that allows no attempt, and it must: the
// refusal records nothing, so on a replay the record at this step is the guest's NEXT call, and a step that read the history first would report that
// call's record as a divergence (and a recorded terminal call or a resume would be replayed and not refused: two rows of the behaviour table pin both).
// Everything else a retried call decides is the dispatcher's or the spec's.
func (s *execSession) DurableCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	// A POLICY THAT ALLOWS NO ATTEMPT IS REFUSED, fresh and replay alike (cleat#4244). maxAttempts arrives across the ABI from every SDK and several
	// pass it unvalidated: the Go SDK sends int64(policy.MaxAttempts), so a cleat.RetryPolicy with no MaxAttempts sent 0. The loop in
	// freshCallWithRetry then never ran, lastErr stayed nil, and the terminal event dereferenced it: a host panic a guest could trigger with
	// ordinary code. The engine is the one place that covers every SDK.
	//
	// FIRST STATEMENT OF THIS BLOCK, which only runs when nothing is stopping the call: bit 31 wins over any field (ABI.md) and a refusal is a field,
	// so a defer segment, a shutdown or a refused event write with a zero policy answers the suspend sentinel below, as for any call. It sits BEFORE
	// the replay branch on purpose, so a call whose terminal event is already recorded, and a resume, are refused too and not replayed. (A call with
	// maxAttempts < 1 panicked before it could record a terminal event, so no such history exists to be broken by that.)
	if !s.stopBeforeNewWork() && maxAttempts < 1 {
		s.engine.log().WarnContext(ctx, "retry policy allows no attempt (maxAttempts < 1); refusing the call",
			"service", service, "operation", operation, "max_attempts", maxAttempts)
		return badParamDurableCall
	}
	return s.step(ctx, m, &retryStep, stepArgs{s1: service, s2: operation, s3: requestJSON, s4: nonRetryableErrorsJSON,
		i1: maxAttempts, i2: initialIntervalMs, i3: backoffCoefficient100x, i4: maxIntervalMs, outPtr: responsePtr, outMaxLen: responseMaxLen})
}

// decodeRetryOutcome answers a replayed retried call from its outcome record, the last of the run the dispatcher read. A recorded refusal is refused again;
// anything else is the recorded call, answered as the plain call answers it (answerRecordedCall).
func (s *execSession) decodeRetryOutcome(c *stepCtx, rec EventRecord) int64 {
	if rec.EventType == EventTypeCallRetryRefused {
		return s.refuseRetryPolicy(c.ctx, c.m, c.args.outPtr, c.args.outMaxLen,
			"retry policy rejected: this policy was refused when this call was recorded, as its worst-case "+
				"backoff exceeds the host-retry budget; run the policy in the guest, suspending between attempts")
	}
	return s.answerRecordedCall(c.ctx, c.m, rec, c.args.outPtr, c.args.outMaxLen)
}

// refuseRetryPolicy answers a guest's retry entry point with RetryPolicyTooLong and the reason, which is the code both
// SDKs branch on to run the policy themselves. No event is recorded and no attempt is consumed: the guest's own loop
// records the attempts, and that is what a replay of them replays.
func (s *execSession) refuseRetryPolicy(ctx context.Context, m api.Module, responsePtr, responseMaxLen uint32, reason string) int64 {
	written, _ := s.writeResult(ctx, m, responsePtr, reason, responseMaxLen)
	return packDurableCallResult(int(written), callErrorRetryPolicyTooLong, 1)
}

// freshCallWithRetry runs the policy: the spec's Fresh. c.Prefix is the attempts a lost worker's FIRST incarnation recorded when the history ended inside
// the policy, so the attempts already spent, the step its idempotency key was derived from and the error of the last one all come from it. The refusal for
// a defer segment, a shutdown, a refused event write and a session that has already suspended is the dispatcher's, applied before this runs and on both
// arrival paths.
func (s *execSession) freshCallWithRetry(c *stepCtx) int64 {
	ctx, m := c.ctx, c.m
	service, operation, requestJSON := c.args.s1, c.args.s2, c.args.s3
	nonRetryableErrorsJSON := c.args.s4
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs := c.args.i1, c.args.i2, c.args.i3, c.args.i4
	responsePtr, responseMaxLen := c.args.outPtr, c.args.outMaxLen
	attemptsSpent, resumeStep, lastRecordedErr := int64(len(c.Prefix)), -1, ""
	if len(c.Prefix) > 0 {
		resumeStep, lastRecordedErr = c.Prefix[0].Step, c.Prefix[len(c.Prefix)-1].Err
	}

	// The policy the GUEST asked for, before the attempt ceiling below trims
	// it. The budget check uses this rather than the clamped value so that the
	// host reaches the same verdict the guest SDKs used to reach on their own
	// -- they computed on their own policy and knew nothing of --max-retries.
	// Judging the clamped policy instead would quietly accept a policy that
	// used to suspend, which is a behaviour change dressed as a refactor.
	requestedAttempts := maxAttempts

	// Worker-enforced ceiling on retry attempts to prevent runaway retries
	// from misconfigured WASM modules.  Use the engine-configured limit if
	// set (it comes from --max-retries on the command line), otherwise the
	// package-level constant.
	ceiling := MaxRetryAttempts
	if s.engine.maxRetries > 0 && s.engine.maxRetries < ceiling {
		ceiling = s.engine.maxRetries
	}
	if maxAttempts > int64(ceiling) {
		maxAttempts = int64(ceiling)
	}
	// A CALL AT THE REPLAY FRONTIER IS A FRESH CALL AND GETS ITS WHOLE POLICY (cleat#4152). The dispatcher leaves replay at the frontier before it gets here,
	// so a retried call that follows the last replayed event directly arrives with replay off and is judged and run as a fresh one: the table's
	// frontier_gets_its_whole_policy row. The history of this bug, kept because the next reader will wonder why the frontier is mentioned at all, is in
	// IMPROVEMENT-PLAN and in that row's header: the old entry did this hand-off itself, and a retried call that skipped it ran ONE attempt.

	// REFUSE A POLICY TOO LONG TO RUN IN ONE SEGMENT, AND RECORD THE REFUSAL. cleat#4032.
	//
	// This check used to sit AFTER the replay return, on the reasoning that "a refusal records no event, so
	// replay must never reach this and find nothing". The premise was right and the conclusion was wrong: a guest
	// that is refused does not stop. Both SDKs fall back to their own loop of ordinary DurableCalls, which DO
	// record events, and on the next segment the guest asks this same question again. Answering from history gave
	// `replayCall` the guest's first FAILED attempt -- the error its own loop was in the middle of retrying --
	// because the replay of a retried call only resumed on call_attempt_failed records and an ordinary call event stopped the
	// walk (replayRetryAttempts, removed when the call moved onto the dispatcher, cleat#4152 G3: the run is now read by retryStep's Role).
	// The workflow died on the very error the policy existed to retry.
	//
	// THE REFUSAL IS NOW AN EVENT, and that is what makes the replay answerable. The two obvious alternatives both
	// fail:
	//
	//   - asking the BUDGET again does not work, because the budget is TENANT STATE and is writable while a run is
	//     in flight (TenantSettings.HostRetryBudget). Measured: a policy refused at 1ns and replayed at 1h fits on
	//     the replay, which then takes the history path and reproduces the defect in the other direction.
	//   - reading the SHAPE alone does not work either, because a host-run policy whose FIRST attempt succeeded also
	//     leaves a lone plain call record. Refusing that one is wrong for every guest without a code-6 fallback --
	//     the AssemblyScript, Java and Python SDKs have none -- and they get a hard failure on the replay of a call
	//     that succeeded.
	//
	// So the refusing segment writes EventTypeCallRetryRefused, and only a replay that FINDS one refuses again. A
	// history written before this change carries no such record: it keeps the old behaviour, which is #4032 itself
	// and cannot be told apart after the fact. New histories are unambiguous in both directions.
	//
	// No attempt is consumed by the refusal itself: the guest's own loop makes the attempts, and recording them is
	// what a replay of them replays.
	//
	// Skipped when the segment is being stopped, because ABI.md requires bit 31 to win over any field and a refusal
	// IS a field: the stop paths below return the sentinel unchanged.
	// A resume is not judged again: the budget was settled before the attempts it is resuming were recorded, and a policy that fitted then fits with fewer
	// attempts left (the old code skipped the check on any session still replaying, which is exactly a resume).
	if len(c.Prefix) == 0 {
		if budget := s.engine.hostRetryBudget(ctx); !retryPolicyFitsBudget(
			requestedAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs, budget) {
			// The REQUEST is deliberately not recorded: nothing reads it back, and a request body is exactly the
			// kind of value the sealed-payload machinery exists for. Service and Op are enough for a divergence
			// report to name the call.
			c.Record(EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCallRetryRefused,
				Service:   service,
				Op:        operation,
			})
			return s.refuseRetryPolicy(ctx, m, responsePtr, responseMaxLen,
				fmt.Sprintf("retry policy rejected: worst-case backoff exceeds the %s host-retry budget; "+
					"run the policy in the guest, suspending between attempts", budget))
		}
	}

	// Parse non-retryable error patterns.
	//
	// A parse failure is refused rather than swallowed. This used to drop the
	// error, which left the slice nil, and a nil slice is indistinguishable from
	// "the author declared no non-retryable errors": isDefinitelyNonRetryable
	// finds nothing to match and every failure becomes retryable. So a workflow
	// that said "do not retry INSUFFICIENT_FUNDS" got its call retried
	// maxAttempts times, which for the non-idempotent operations that
	// declaration exists to protect is a duplicate side effect.
	//
	// Note that the interface check in isDefinitelyNonRetryable short-circuits
	// ahead of the pattern list, so this only bites errors classified by message
	// -- which is exactly what this argument is for.
	//
	// nonRetryableErrorsJSON arrives across the ABI from five language SDKs, the
	// layer CLAUDE.md records as the source of four real defects. An SDK sending
	// a bare comma-separated string rather than a JSON array lands here.
	//
	// Failing closed is the safe direction: "I could not read your safety
	// declaration" must not be treated as "you made no safety declaration".
	// badParamDurableCall rather than errBadParam because the guest adapter
	// decodes the packDurableCallResult layout -- see memory.go for what the raw
	// sentinel did to it.
	var nonRetryableErrors []string
	if nonRetryableErrorsJSON != "" {
		if err := json.Unmarshal([]byte(nonRetryableErrorsJSON), &nonRetryableErrors); err != nil {
			s.engine.log().WarnContext(ctx, "unparseable non-retryable error list; refusing the call",
				"service", service, "operation", operation, "error", err)
			c.RecordsNothing("a non-retryable list that cannot be read is refused before any attempt: failing closed, and nothing is recorded")
			return badParamDurableCall
		}
	}

	var lastErr error
	exhausted := true

	// A resume whose recorded attempts already equal (or exceed) this incarnation's maxAttempts never enters the loop below, and lastErr stayed nil:
	// the terminal event was then built from a nil error and the call panicked (cleat#4244). It is reachable when the resume's policy is shorter than
	// the one recorded (a worker restarted with a lower --max-retries). The policy IS spent, so the honest outcome is the one it would have reached:
	// the retries-exhausted failure, carrying the error of the last attempt that was recorded. The recorded class is not carried (only the message
	// is read back here), so the terminal event's error class is empty.
	if attemptsSpent >= maxAttempts && attemptsSpent > 0 {
		if lastRecordedErr == "" {
			lastRecordedErr = "the last recorded attempt carried no error text"
		}
		lastErr = errors.New(lastRecordedErr)
	}

	// One step for every attempt, so every attempt carries the same idempotency
	// key. That is the intent: a retry of a call that may already have been
	// performed is exactly the case a key exists to collapse.
	retryStep := s.stepCount
	if resumeStep >= 0 {
		// Resuming a policy a crash interrupted: keep the step the FIRST
		// incarnation used, so every attempt across every incarnation still
		// carries one idempotency key. Without this the key changes at exactly
		// the moment a duplicate is most likely.
		retryStep = resumeStep
	}

	// Attempts spent before this incarnation, read from the recorded
	// call_attempt_failed events by the replay path above. Starting at 1
	// unconditionally is what made MaxAttempts a per-incarnation bound: a
	// 3-attempt policy crashed mid-backoff made four real calls, measured
	// against a control of three (cleat#1145). A caller's MaxAttempts is a
	// bound on how many times a side effect may be attempted, and a crash is
	// not consent to exceed it.
	for attempt := attemptsSpent + 1; attempt <= maxAttempts; attempt++ {
		// Every attempt after the first is new work that goes on top of whatever the loop has recorded so far. A failed attempt's event is written
		// BEFORE its backoff, and a refused write (cleat#4160) marks the session stopped, but that mark is only read at a host call's entry, and this
		// loop makes several service attempts inside ONE host call. Without this check the loop performed the next attempt for real, reported it to
		// the guest, and left no trace of it in event_history, so a reclaim performed it again (cleat#4210, found by the fault-injection harness).
		// The first attempt was checked at the call's entry above; the backoff's own select already returns the stop on shutdown.
		if attempt > attemptsSpent+1 && s.stopBeforeNewWork() {
			return callSuspendSentinel
		}
		resp, callErr := s.callService(ctx, service, operation, requestJSON, retryStep)

		if callErr == nil {
			// Success — record one event and return.
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCall,
				Service:   service,
				Op:        operation,
				Request:   requestJSON,
				Response:  resp,
			}
			c.Record(rec)

			written, writtenEC := s.writeOut(ctx, m, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), writeErrorClass(writtenEC), writtenEC)
		}

		lastErr = callErr

		// Check if error is definitively non-retryable.
		if isDefinitelyNonRetryable(callErr, nonRetryableErrors) {
			exhausted = false
			break
		}

		if attempt < maxAttempts {
			// This failure is about to trigger another attempt -- record it as
			// a retry. Nothing else in this loop is visible in Prometheus: the
			// only event history entry freshCallWithRetry writes is the final
			// success or failure, so a call that failed and retried 50 times
			// before succeeding looks identical, from the metrics alone, to one
			// that succeeded on the first try. A retry storm should be visible
			// as a rate, not require reading event history after the fact.
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordCallRetry(ctx,
					attribute.String("service", service),
					attribute.String("operation", operation),
				)
			}

			// Record the spent attempt BEFORE the backoff, because the backoff
			// is where the worker is lost. Recorded only when another attempt
			// follows: the final failure falls out of this loop and is carried
			// by the terminal call event below, so a history never ends with an
			// attempt event unless the run was interrupted -- which is the
			// signal the replay path reads (cleat#1145).
			c.Record(EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCallAttemptFailed,
				Service:   service,
				Op:        operation,
				Attempt:   int(attempt),
				Err:       callErr.Error(),
			})

			// Exponential backoff using host time (not DurableSleep).
			//
			// `maxIntervalMs > 0` is the whole of this fix, and its absence was
			// not a small bug: MaxInterval is an OPTIONAL field on RetryPolicy,
			// so a policy that leaves it at its zero value had every backoff
			// clamped to 0 and then raised to the 1ms floor below. Retries
			// happened, immediately, and the configured InitialInterval was
			// silently ignored. Measured: six attempts at a 2s interval
			// completed in 384ms.
			//
			// retryPolicyFitsBudget, in this same file, always had the guard --
			// `if maxInterval > 0 && interval > maxInterval`. So the budget
			// check and the executor disagreed about what the same policy
			// meant: the check computed the worst case from the unclamped
			// intervals and could reject a policy as too long, while the
			// executor would have run it in a millisecond per attempt. Zero
			// means "no maximum" in one and "maximum of zero" in the other.
			// The factor is truncated to a whole number BEFORE it multiplies, as it always was (a coefficient of 1.5 repeats the first interval), and the
			// product is bounded: initialIntervalMs * int64(math.Pow(...)) overflowed int64, and int64() of an out-of-range float is
			// implementation-defined (cleat#4105).
			backoffMs := boundedBackoffMs(initialIntervalMs, math.Pow(float64(backoffCoefficient100x)/100.0, float64(attempt-1)))
			if maxIntervalMs > 0 && backoffMs > maxIntervalMs {
				backoffMs = maxIntervalMs
			}
			if backoffMs < 1 {
				backoffMs = 1 // minimum backoff to prevent a tight retry loop
			}
			// THE WAIT IS WORKER-LOCAL, AND THAT IS A DECISION (2026-09-10,
			// cleat#1111). This is host memory: a worker lost during the
			// backoff discards the remaining wait, and the reclaimed run
			// retries as soon as the reaper releases it rather than at the time
			// this policy implied. Measured on the port harness at a 20s
			// interval: 19.6s between attempts uninterrupted, 11.4s across a
			// worker kill -- the reaper's latency, not the interval.
			//
			// Declined: making the wait durable here. The reclaim delay is
			// already latency the policy did not ask for, and re-waiting the
			// untaken remainder on top would make a crash cost more than the
			// outage that caused it. A backoff spaces attempts against a
			// dependency; it is not a promise about elapsed time.
			//
			// The SDK-level loop in cleat/runtime.go -- taken by policies above
			// hostRetryBudget -- backs off with DurableSleep and DOES survive,
			// because its deadline is in the history. So --host-retry-budget is
			// the boundary for this property as well as for slot-holding, which
			// its flag help now says, and which
			// docs/operations/workflow-retention.md states for operators.
			//
			// NOT covered by that decision, and still a defect: no event is
			// recorded for a failed attempt (recordEvent above fires only on
			// success), so a reclaimed run restarts the policy from attempt one
			// and re-spends MaxAttempts. Measured at 4 calls for a 3-attempt
			// policy across a crash, against 3 uninterrupted. See cleat#1145.
			select {
			case <-ctx.Done():
				// errCode 0 here reported a *successful* call with an empty
				// response: the guest's generated adapter branches on
				// `errCode != 0`, so a retry loop abandoned mid-backoff looked
				// to the workflow like the service had answered with "".
				written, _ := s.writeResult(ctx, m, responsePtr, ctx.Err().Error(), responseMaxLen)
				return packDurableCallResult(int(written), callErrorUnknown, 1)
			case <-s.engine.shutdownRequested:
				// cleat#2020: ctx here is context.Background()-derived (every
				// wasmtime host function builds its own), so ctx.Done() above
				// can never fire on a real worker shutdown -- this is the
				// channel that actually does.
				//
				// The guest is told to STOP, not that the call failed
				// (cleat#2285). This used to hand it a retryable failure,
				// which a guest is free to turn into anything: a workflow that
				// compensates on error ran its compensation, and finished
				// COMPLETED, on a fault that never happened. Nothing about
				// this attempt failed; this worker is going away. The worker
				// releases the run whatever comes back (see executeWorkflow),
				// so the sentinel only has to make the guest unwind without
				// doing anything more.
				return callSuspendSentinel
			case <-time.After(guestMsDuration(backoffMs)):
			}
		}
	}

	// All retries exhausted or non-retryable error — record failure event.
	errMsg := lastErr.Error()
	if exhausted {
		errMsg = "retries exhausted: " + errMsg
	}
	// The loop broke early precisely because this error is not worth retrying.
	// Reporting it as retryable told the workflow to do the one thing the
	// engine had just decided against -- and for a non-idempotent operation a
	// caller marks non-retryable, that is a duplicate side effect.
	//
	// Recording the bit is what makes fixing it legal: replay reads this event
	// back, and if it could not recover the classification the same step would
	// be non-retryable on the first run and retryable on the replay of it.
	nonRetryable := !exhausted
	rec := EventRecord{
		Step:            s.stepCount,
		EventType:       EventTypeCall,
		Service:         service,
		Op:              operation,
		Request:         requestJSON,
		Err:             errMsg,
		ErrNonRetryable: nonRetryable,
		// The class the caller supplied, kept alongside the bit the engine
		// acted on. IMPROVEMENT-PLAN 2.35: without this the taxonomy is
		// collapsed at write time and replay can only re-derive the bit.
		ErrCode: recordedErrorClass(lastErr),
		// The engine's own conclusion, recorded because nothing else carries
		// it. `exhausted` is known here and nowhere downstream: the message
		// goes to the GUEST as a plain string, so by the time the worker
		// decides dead-lettering there is no classification left to read.
		// That is what made the decision a substring match. cleat#902.
		RetriesExhausted: exhausted,
	}
	c.Record(rec)

	// Retries exhausted stays callFailureCode: the error was retryable, the
	// attempts simply ran out, and calling again later may well work.
	written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
	return packDurableCallResult(int(written), recordedFailureCode(nonRetryable), 1)
}

func (s *execSession) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	// A sleep after a suspension would REPLACE the first suspension with its own (suspendFor assigns), so a run waiting on a child would wake at the sleep's
	// deadline instead. It answers its own suspend status, which its decoders already test (cleat#4285).
	if res, ok := s.refuseIfSuspended("DurableSleep", packSleepResult(sleepStatusSuspend, durationMs)); ok {
		return res
	}
	// Sleep is local: it records no event. So what tells us whether a sleep has
	// already been served is not history but time.
	//
	// The anchor is the timestamp of the last event the workflow recorded -- a
	// real moment, written when that step ran. Every sleep since then pushes a
	// virtual deadline further past it. If that deadline is already behind real
	// time, the wait has happened, whether it was spent suspended, sitting in a
	// queue, or with the worker down. If it is still ahead, this is a genuinely
	// new wait and the workflow suspends for the remainder.
	//
	// This replaces the replayJustEnded flag, which asked a question sleep
	// cannot answer for itself: "did some *other* durable call just cross the
	// replay frontier?" In a faithful replay none does -- every earlier
	// operation was recorded and replay-matches -- so the sleep was always what
	// reached the end of history, and it re-suspended forever. It also replaces
	// the narrower frontier check that fixed only the single-sleep case.
	// IMPROVEMENT-PLAN 3.67.
	//
	// max() rather than assignment, and this is the subtle part: Now() reads
	// history[stepCount-1] while stepCount is within history, and sleeps do not
	// advance stepCount. Two sleeps in a row would otherwise read the *same*
	// anchor, compute the same deadline, and the second would complete a wait
	// it never performed -- the same "completing on someone else's evidence"
	// failure as the bug this replaces, just one step further along.
	anchor := s.nowMs
	if n := s.Now(ctx); n > anchor {
		anchor = n
	}
	if anchor <= 0 {
		// No anchor: a fresh workflow that has recorded nothing yet, and a
		// nowMs seed that was never set. The package-level seed is refreshed by
		// the worker's dispatch loop but by nothing in the CLI and embedded
		// paths, where it stays zero -- and an anchor at the epoch puts every
		// deadline decades in the past, so every sleep would complete instantly
		// and no workflow run under `cleat run` or cleatctl would ever wait.
		anchor = s.engine.realNowMs()
	}
	// clampGuestMs: anchor + i64::MAX wraps negative and reads as a deadline long past, so a sleep of "forever" completed at once (cleat#4105).
	s.nowMs = anchor + clampGuestMs(durationMs)

	if s.nowMs <= s.engine.realNowMs() {
		return packSleepResult(sleepStatusCompleted, 0)
	}

	// The park-or-suspend choice is the helper's (cleat#4152, W2b). A refusal sets exactly the suspension this call set inline before.
	//
	// A timer's outcome is ADVISORY and the CLOCK decides. A sleep records nothing and is served by the clock on replay (above), so completing it
	// before its deadline would let the run go on and record events after a sleep that a replay, reading the same clock, still finds waiting: the
	// replay would suspend at the sleep with those events unread behind it. So a wake or a passed deadline completes the sleep only if the clock
	// agrees; a wake that came early suspends with the same request, as a refusal would have.
	req := parkRequest{
		Kind:       wakeTimer,
		DeadlineMs: s.nowMs,
		Reason:     fmt.Sprintf("cleat_sleep(%dms)", durationMs),
		Until:      time.UnixMilli(s.nowMs),
	}
	if s.parkOrSuspend(ctx, req) != parkRefused {
		if s.nowMs <= s.engine.realNowMs() {
			return packSleepResult(sleepStatusCompleted, 0)
		}
		s.suspendFor(req) // woken early: the clock says it is not time
	}

	return packSleepResult(sleepStatusSuspend, durationMs)
}

// ServeWasiSleep implements HostHandler. See that interface for why this is
// not DurableSleep and must not become it.
//
// cleat#1633. The anchor logic is DurableSleep's, and its reasoning applies
// unchanged: max() rather than assignment, because Now() reads
// history[stepCount-1] while stepCount is within history and sleeps do not
// advance stepCount -- so two sleeps in a row would otherwise read the same
// anchor and the second would complete a wait it never performed.
func (s *execSession) ServeWasiSleep(ctx context.Context, durationMs int64) time.Duration {
	if durationMs <= 0 {
		return 0
	}

	anchor := s.nowMs
	if n := s.Now(ctx); n > anchor {
		anchor = n
	}
	if anchor <= 0 {
		// No anchor: a fresh run that has recorded nothing and whose nowMs seed
		// was never set, which is the CLI and embedded paths. An anchor at the
		// epoch puts every deadline decades in the past, so every sleep would
		// report "already waited" and nothing would ever block. Same reasoning
		// as DurableSleep's own zero-anchor branch.
		anchor = s.engine.realNowMs()
	}
	s.nowMs = anchor + clampGuestMs(durationMs)

	remainingMs := s.nowMs - s.engine.realNowMs()
	if remainingMs <= 0 {
		// The deadline is already behind real time, so the wait has happened --
		// whether it was spent replaying, queued, or with the worker down.
		// Replay and resumed-after-downtime are the same case here, which is
		// the property that makes this predicate right rather than a flag.
		return 0
	}
	return time.Duration(remainingMs) * time.Millisecond
}

func (s *execSession) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	return s.step(ctx, m, &deferStep, stepArgs{s1: description, outPtr: deferIDPtr, outMaxLen: deferIDMaxLen})
}

// deferStep is DurableDefer as a step (cleat#4152, G1a).
//
// REFUSED ONLY IN AN ARMED SEGMENT, NOT LIKE THE OTHER EFFECT CALLS. A segment over a killed run stops at a defer registration the same way it
// stops at a call: the registration is in the history, so this is not a new one, it is the stop point. Only in that segment: an ordinary
// segment's body past its frontier has always been allowed to register a defer, and refusing it is not this change's to do (cleat#4079). The
// old code also registered after a refused event write and on shutdown, where SideEffect and every other effect call is refused; that is
// recorded by the characterization table and kept (Refuse below), not decided here.
//
// A registration replayed from history RE-REGISTERS the defer, it does not just re-answer: this is the only branch a defer registered in an
// earlier segment ever reaches again (every later segment replays past its registration), so skipping the map write dropped the defer
// permanently. The id is recomputed before the step is consumed, and a record written before DeferID was recorded falls back to what the fresh
// path would have minted at that step, so a history written long ago still hands the guest the id it was first given.
//
// Identity: NoIdentity. The description is a guest-chosen label that the record carries and replay re-registers from (the record's wins, the
// argument is only the fallback for a record written before descriptions were recorded); it can legitimately differ between a run and its
// replay, and the id is generated from the step. The reviewer decides (Revision 3 of the design).
var deferStep = stepSpec{
	Name:               "defer",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypeDefer,
	NoIdentity:         "the description is a guest-chosen label that the record carries and replay re-registers from; the id is generated from the step",
	Refuse:             func(s *execSession) bool { return s.stopArmed && s.stopBeforeNewWork() },
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		s := c.s
		// The step has been consumed when Decode runs, so the step the record held is stepCount-1; the fallback is what the fresh path below
		// would have minted at that same step.
		deferID := rec.DeferID
		if deferID == "" {
			deferID = fmt.Sprintf("defer-%d", s.stepCount-1)
		}
		desc := rec.DeferDescription
		if desc == "" {
			desc = c.args.s1
		}
		s.mu.Lock()
		s.deferrals[deferID] = desc
		s.mu.Unlock()

		written, writtenEC := s.writeOut(c.ctx, c.m, c.args.outPtr, deferID, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
	Fresh: func(c *stepCtx) int64 {
		s := c.s
		deferID := fmt.Sprintf("defer-%d", s.stepCount)
		c.Record(EventRecord{
			Step:             s.stepCount,
			EventType:        EventTypeDefer,
			DeferDescription: c.args.s1,
			DeferID:          deferID,
		})
		s.mu.Lock()
		s.deferrals[deferID] = c.args.s1
		s.mu.Unlock()

		written, writtenEC := s.writeOut(c.ctx, c.m, c.args.outPtr, deferID, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

func init() { registerStepSpec(&deferStep) }

// DurableLog emits a guest log line. It is NOT durable, despite the name and
// despite three documents saying otherwise. cleat#1308.
//
// WHAT WAS WRONG. The body was `return 0`: the message was read across the
// WASM boundary by both backends and dropped on the floor. The comment that
// used to sit here -- "Log output goes via the worker's stdout/stderr capture"
// -- described something that did not happen, because nothing anywhere printed
// the message.
//
// THAT IS WORSE THAN A MISSING FEATURE, because a linter rule pushes authors
// into it. docs/workflow-go-constraints.md blocks `fmt.Println` under E015 --
// "output to stdout/stderr is not captured reliably during replay" -- and
// tells the author to use `h.DurableLog()` instead, on the grounds that it
// "records log output in event history and replays it deterministically". So
// an author following the linter replaced a call that printed with one that
// did nothing at all, and lost their logging.
//
// WHAT THIS DOES NOT DO, deliberately: record an event. That is the open half
// of cleat#1308 and it is a replay-compatibility decision, not an omission.
// Replay matching is positional -- advanceReplayStep consumes
// s.history[s.stepCount] -- so a workflow that logs inside a loop and is
// replayed against a history recorded before such a change would consume the
// wrong events from that point on. Everything else the feature needs already
// exists (EventTypeDurableLog, its compaction code and both codec directions,
// the Message/LogLevel/LogKV fields and their payload carrier); the missing
// piece is a recording path that in-flight runs survive.
//
// isReplay is checked so a replayed run does not re-emit lines the original
// already emitted. That is the same reason a durable call is not re-issued,
// applied to output rather than to effects -- and it is the one determinism
// property this call does have.
func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	if s.isReplay {
		return 0
	}
	s.engine.log().InfoContext(ctx, message,
		"workflow_id", s.workflowID, "tenant_id", s.tenantID,
		"step", s.stepCount, "source", "workflow")
	return 0
}

func (s *execSession) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	return s.step(ctx, m, &durableSendStep, stepArgs{s1: service, s2: operation, s3: requestJSON})
}

// durableSendStep is DurableSend as a step (cleat#4152, G1b). A fresh send is new work: it dispatches a request to an external service
// through s.engine.caller, on a goroutine that outlives this call, and a terminated workflow would cause a side effect after its own
// end, so the dispatcher refuses it where the call did.
//
// IDENTITY IS NEW. The old replay consumed any durable_send record, so a send to another service or operation replayed as the recorded
// one. A send is identified by its service and operation (the design's "target and message name"); the request text is not compared,
// which the old code did not either.
var durableSendStep = stepSpec{
	Name:               "durable_send",
	NoDivergenceDetail: "its old message was `replay divergence: expected durable_send, got a different event`: less than the shared one, and no request; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypeDurableSend,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.Service != a.s1 || rec.Op != a.s2 {
			return fmt.Sprintf("send %s.%s, recorded %s.%s", a.s1, a.s2, rec.Service, rec.Op)
		}
		return ""
	},
	// fire-and-forget is recorded and not re-executed: a replayed send answers success and dispatches nothing.
	Decode: func(c *stepCtx, rec *EventRecord) int64 { return 0 },
	Fresh: func(c *stepCtx) int64 {
		s, ctx, service, operation, requestJSON := c.s, c.ctx, c.args.s1, c.args.s2, c.args.s3

		rec := EventRecord{
			Step:      s.stepCount,
			EventType: EventTypeDurableSend,
			Service:   service,
			Op:        operation,
			Request:   requestJSON,
		}
		c.Record(rec)

		// Capture the step before the goroutine starts. Reading s.stepCount from inside it would race with the session advancing, and
		// would key the call to whatever step happened to be current when the goroutine was scheduled.
		sendStep := rec.Step

		// Execute the fire-and-forget call through the caller. Wrapped in a timeout context to bound the goroutine's lifetime in case the
		// external Call blocks indefinitely.
		if s.engine.caller != nil {
			go func() {
				if ctx.Err() != nil {
					return
				}
				// cleat#2020: ctx is context.Background()-derived, so ctx.Err() above can never observe a real worker shutdown; this is
				// the channel that does. Checked once, like ctx.Err(), because this is a pre-dispatch guard, not a wait -- an event was
				// already recorded above, so a shutdown after this point is the same unsent-request case the surrounding fire-and-forget
				// contract already accepts.
				select {
				case <-s.engine.shutdownRequested:
					return
				default:
				}
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				_, _ = s.callService(callCtx, service, operation, requestJSON, sendStep)
			}()
		}
		return 0
	},
}

func init() { registerStepSpec(&durableSendStep) }

func (s *execSession) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			// Same check, same reason as DurableSend above. cleat#1507.
			if rec.EventType != EventTypeDurableScheduleInvoke {
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordReplayFailure(ctx)
				}
				s.engine.log().ErrorContext(ctx,
					"replay divergence: expected durable_schedule_invoke, got a different event",
					"workflow_id", s.workflowID, "step", rec.Step,
					"expected", EventTypeDurableScheduleInvoke, "actual", rec.EventType,
					"service", service, "operation", operation)
				// errCode 1, not callErrorUnknown -- which is 0, i.e. SUCCESS.
				// wasm/adapter_metadata.go decodes this return as
				// `errCode := uint32(result)` and raises an error only when it
				// is non-zero, so the guest would have been told the send
				// succeeded. Matches the divergence code AwaitChild and
				// AwaitAnyChild use.
				return packSimpleResult(1)
			}
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			return 0
		}
		// Past recorded history -- switch to fresh execution. Same defect and
		// same fix as DurableSend above.
		s.exitReplay()
	}

	// A fresh schedule_invoke is new work, and it outlives the segment by
	// design: the host records a delayed invocation that fires after the delay,
	// so a terminated workflow would keep causing side effects on a timer.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	if res, ok := s.refuseIfSuspended("DurableScheduleInvoke", callSuspendSentinel); ok {
		return res
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeDurableScheduleInvoke,
		Service:    service,
		Op:         operation,
		Request:    requestJSON,
		DurationMs: delayMs,
	}
	s.recordEvent(rec)

	// As in DurableSend: capture before the goroutine, not inside it.
	scheduleStep := rec.Step

	// Schedule the call. For now, run in a goroutine after the delay.
	// Wrap the call in a timeout context to bound goroutine lifetime
	// in case the external Call blocks indefinitely.
	if s.engine.caller != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-s.engine.shutdownRequested:
				// cleat#2020: ctx.Done() above can never fire on a real worker
				// shutdown (ctx is context.Background()-derived); this aborts
				// the delayed dispatch instead of waiting out the delay on a
				// worker that is already going away.
				return
			case <-time.After(guestMsDuration(delayMs)):
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				_, _ = s.callService(callCtx, service, operation, requestJSON, scheduleStep)
			}
		}()
	}
	return 0
}

// SetDeferPhase records that the guest has started or finished draining its
// defer table.
//
// WHY THE GUEST HAS TO TELL US. On the ordinary failure path the guest's own
// generated wrapper runs the registered defer bodies -- see wasm/exports.go's
// _cleatRunDeferred, and engine/executor.go, which explains why the host must
// NOT also invoke them. So the host is not in the loop, and a defer's host
// calls arrive through exactly the same path as the body's. They were
// indistinguishable in the history, and cleat#1155 is what that cost: a
// workflow that exhausted its retries and then cleaned up was classified
// `failed` rather than `dead_lettered`, because the cleanup's own durable call
// was the last event and dead-lettering asks what the last durable act was.
//
// The guest already knew -- every SDK tracks this internally to refuse
// registration from inside a defer body. This reports what it already has.
//
// IT RECORDS NO EVENT, and that is the design rather than an economy. Replay
// is positional: an event here would consume a step, and any workflow already
// in flight when this shipped would replay a history whose step N is not the
// event the new guest emits. A flag on events that are recorded anyway shifts
// nothing, and a history written before this existed simply carries no flags.
//
// Idempotent and reentrant-safe by being a plain assignment: a guest that
// reports the phase twice, or never reports the end because it trapped, leaves
// the session in a state that only affects events recorded after it -- and a
// trapped guest records nothing more.
func (s *execSession) SetDeferPhase(_ context.Context, on bool) int64 {
	s.mu.Lock()
	if on && !s.inDeferPhase {
		s.deferPhaseBeganSuspended = s.suspendErr != nil
	}
	s.inDeferPhase = on
	s.mu.Unlock()
	return 0
}

// guestIsDrainingDefers reports whether the guest has told the host it is
// running its own defer bodies (SetDeferPhase). Read by the component
// dispatchers for the child awaits, which must not stop a guest that is in the
// middle of its cleanup (cleat#3429).
func (s *execSession) guestIsDrainingDefers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inDeferPhase
}
