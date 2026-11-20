package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// The replay step dispatcher, cleat#4152; the design is docs/design/replay-step-dispatcher.md and this file is its PR 1.
//
// Every host call that records an event used to answer five questions in its own words: where is this call (history, the frontier, past it),
// does the recorded event belong to it, may it start new work here, what does it record and when, and what does the guest get back. The bugs
// of this class (#4143, #4152, #4153, #4157, #4158, #4159, #4177) were each one call answering one of them differently from its neighbours.
// step answers them once, for the calls that have been moved onto it; a host call becomes a stepSpec, a description of one step.
//
// THE RULE FOR EVERY CALL MOVED ONTO step: the move changes where the code lives and nothing else. The events a call records, their order,
// the checksum chain, the guest-visible result and the suspension are the same as before; a characterization table recorded from the old
// code pins that (a_promise_single_step_behaves_as_recorded_before_the_dispatcher_test.go).
//
// What is NOT here, and why:
//   - A park-or-suspend stage. It is for Wait steps (PR 6) and has no consumer yet; adding a field nothing reads would be a claim, not a
//     mechanism. The design names it and the spec grows it when the first Wait moves.
//   - Pre-checks (cancellation poll, canStartNewWork, the event cap). DurableCall carries them; the three calls here do not. They arrive as
//     spec fields with DurableCall.
//   - Failure handling for a failed event write. It stays inside recordEvent (stopAfterFailedEventWrite orders the quota path before it sets
//     eventWriteFailure, behind a re-entrancy latch). step never looks at the persistence outcome: a failed write stops the guest through
//     stopBeforeNewWork on the NEXT step.

// stepEffect says what a step does to the outside world. The only thing it decides today is whether stopBeforeNewWork applies.
//
// Two of the design's four classes are not here yet: Read (decided per call, section 5) and Terminal (ContinueAsNew). A value nothing uses is
// a claim and not a mechanism, so each arrives with the first spec that needs it.
type stepEffect int

const (
	// effectPure: derived from the session and the history; no event, no world access. Pure calls do not go through step at all; the value
	// exists so that validate can refuse a spec that declares it and records, and so a spec table can say so.
	effectPure stepEffect = iota
	// effectEffect: reaches the outside world or changes durable state. Refused in a segment's body.
	effectEffect
)

// recordShape is how many events the fresh side of a step records.
type recordShape int

const (
	recordsOne  recordShape = iota // exactly one event
	recordsNone                    // none (a Lookup, a Live read)
	recordsMany                    // one or more (Multi, Stream, Wait); the spec says how many in its own test
)

// recordRole says how one record relates to a multi-record step: the role it plays in the run of records the step owns.
type recordRole int

const (
	// roleForeign: not this step's record. It ends the run, and what that means depends on what came before it (see replayRun).
	roleForeign recordRole = iota
	// rolePrefix: a record that continues the step without finishing it: a failed attempt of a retried call, a pending await.
	rolePrefix
	// roleFinal: the record that finishes the step: the call's outcome, the acquisition, the await's resolution.
	roleFinal
)

// stepArgs is the per-call data a spec needs. A spec is a package-level value and holds no closure over the call, so a step costs no
// allocation beyond this struct (section 9, risk 5, of the design); two strings cover the promise calls and the next groups add fields.
type stepArgs struct {
	s1, s2 string
	// outPtr and outMaxLen are where a divergence message is written, and where the call's own output goes; zero when it has none.
	outPtr, outMaxLen uint32
}

// stepCtx is what a spec's Decode and Fresh are handed.
type stepCtx struct {
	ctx  context.Context
	m    api.Module
	s    *execSession
	args stepArgs
	// recorded counts events recorded through Record, so the dispatcher can check them against the spec's declared shape.
	recorded int
	// Prefix is the run of prefix records a multi-record step consumed when history ENDED INSIDE its run, handed to Fresh so it can continue
	// from there. It is data: the spec decides what it means (a retried call reads len(Prefix) as attempts already spent; a heartbeat
	// ignores it). nil for a step that did not end inside a run.
	Prefix []EventRecord
}

// Record records an event where the call records it today. It is the only way a spec's Fresh writes history, which is what lets the
// dispatcher check afterwards that it wrote what the spec declared. Calls differ on WHEN they record (a promise create records first and
// writes its store after), so Fresh calls this where its call did and the dispatcher imposes no order.
func (c *stepCtx) Record(rec EventRecord) eventPersistence {
	c.recorded++
	return c.s.recordEvent(rec)
}

// stepSpec is a description of one host call's step.
type stepSpec struct {
	Name    string
	Effect  stepEffect
	Records recordShape
	// Want is the type of the event this step consumes on replay. A record of any other type is a divergence, reported once, in one format.
	Want EventType
	// Identity compares what identifies THIS call with what the record at this step names, and returns "" when they are the same call or a
	// short description of the difference when they are not (a different promise, lock, signal, child). A record of the right type that names
	// something else is a divergence, not an answer (cleat#4211): without this a replay accepts another call's record, consumes it and answers
	// as though it were this one. The fields that identify a call are the ones that cannot legitimately vary between a run and its replay;
	// a payload that can (a timestamp in a request, a generated id) is not identity.
	Identity func(a stepArgs, rec *EventRecord) string
	// Refuse says when this step is refused instead of run, on both arrival paths. nil means stopBeforeNewWork, which is every spec that
	// had no reason to differ. A spec whose old code refused under a narrower condition says so here (DurableDefer refuses only in a defer
	// segment over a killed run, because an ordinary segment's body has always been allowed to register a defer past its frontier,
	// cleat#4079): the dispatcher must not newly refuse what the call did not. Not allowed on a Pure step, which is never refused.
	Refuse func(s *execSession) bool
	// StaysInReplayAtFrontier is for a lookup that answers a question about the record at the replay frontier without ending replay: it does
	// not call leaveReplay there, still applies the refusal, and calls Fresh with replay still on, so the step that follows does the leaving
	// (SideEffectRecorded asks, then SideEffect decides). Only for a step that records nothing and is not Pure.
	StaysInReplayAtFrontier bool
	// NoIdentity says why a call has no identifying field, when it has none: exactly one of Identity and NoIdentity is set, so "no identity"
	// is a decision with a reason that a reviewer can read, not an omission.
	NoIdentity string
	// Decode turns the consumed record into the call's result. It must not touch the world. A single-record step sets Decode; a multi-record
	// step (Role set) sets DecodeRun instead.
	Decode func(c *stepCtx, rec *EventRecord) int64
	// Role makes this a MULTI-RECORD step: one call whose history is a run of consecutive records, any number of rolePrefix records ending in a
	// roleFinal one (a retried call's failed attempts then its outcome; SetScope's failed acquisitions then the acquisition; an await's
	// pending record then its resolution). nil means a single-record step. Role classifies a record by its type and shape only; WHAT it
	// names (the key, the service and op) is Identity's, applied to every record of the run. See replayRun for what each ending means.
	Role func(a stepArgs, rec *EventRecord) recordRole
	// DecodeRun turns a complete run into the call's result: the records in order, the last of them the roleFinal one, or, for a spec with
	// EndsWithoutFinal, prefix records only. It may keep a replay-time write its call made today (a pending call's ambiguity record) and
	// the spec's comment says so; otherwise it must not touch the world.
	DecodeRun func(c *stepCtx, run []EventRecord) int64
	// EndsWithoutFinal says a run of prefix records followed by a FOREIGN record is a complete run with no final: an await that was recorded
	// and then not answered (it timed out). Without it that shape is a divergence, as for a retried call or SetScope.
	EndsWithoutFinal bool
	// Fresh does the work and records, through c.Record, where the call records. It is called only once the dispatcher has decided
	// fresh work is allowed.
	Fresh func(c *stepCtx) int64
}

// stepSpecs lists every spec in the engine. It exists for the dispatcher's tests (step_dispatcher_test.go), which run each property over every
// spec on it and check the list against the `var x = stepSpec{` declarations in the source, so a spec nobody registered is on no list and is
// caught. A host call moved onto step adds its spec here in the same change.
var stepSpecs = []*stepSpec{&createPromiseStep, &resolvePromiseStep, &rejectPromiseStep}

// validate says whether a spec is well-formed: it names a replay event, can decode it and can run fresh, and a step that declares itself Pure
// (and so is never refused in a segment) records nothing.
func (sp *stepSpec) validate() error {
	switch {
	case sp.Name == "":
		return fmt.Errorf("a step spec has no name")
	case sp.Want == "":
		return fmt.Errorf("step spec %q has no Want: it names no event to consume on replay", sp.Name)
	case sp.Fresh == nil:
		return fmt.Errorf("step spec %q is missing its Fresh", sp.Name)
	case sp.Role == nil && (sp.Decode == nil || sp.DecodeRun != nil || sp.EndsWithoutFinal):
		return fmt.Errorf("step spec %q is a single-record step and needs exactly a Decode (no DecodeRun, no EndsWithoutFinal)", sp.Name)
	case sp.Role != nil && (sp.DecodeRun == nil || sp.Decode != nil):
		return fmt.Errorf("step spec %q is a multi-record step (it has a Role) and needs exactly a DecodeRun, not a Decode", sp.Name)
	case sp.Role != nil && sp.Records == recordsNone:
		return fmt.Errorf("step spec %q is a multi-record step that records nothing: a run is made of recorded events", sp.Name)
	case (sp.Identity == nil) == (sp.NoIdentity == ""):
		return fmt.Errorf("step spec %q must set exactly one of Identity and NoIdentity: a call either names what identifies it or says why it has nothing to compare", sp.Name)
	case sp.Refuse != nil && sp.Effect == effectPure:
		return fmt.Errorf("step spec %q is Pure and has a Refuse: a Pure step is never refused", sp.Name)
	case sp.StaysInReplayAtFrontier && (sp.Records != recordsNone || sp.Effect == effectPure):
		return fmt.Errorf("step spec %q stays in replay at the frontier but records or is Pure: only a non-Pure step that records nothing may answer without leaving replay", sp.Name)
	case sp.Effect == effectPure && sp.Records != recordsNone:
		return fmt.Errorf("step spec %q is Pure and declares it records events: a Pure step is not refused in a segment, so it must record nothing", sp.Name)
	}
	return nil
}

// A malformed spec is a programming error, so it stops the process at start-up, as a bad regexp does, and not at the first workflow that
// reaches the call. The dispatcher's tests run the same check and the registry against the source.
func init() {
	for _, sp := range stepSpecs {
		if err := sp.validate(); err != nil {
			panic(err)
		}
	}
}

// step runs one host call as a step.
//
//	replay, at a record  -> the record must be Want, else a divergence; consume it; Decode
//	replay, at the end   -> leave replay (asserting the history was all read), then as fresh
//	any non-Pure step    -> refused (callSuspendSentinel) when stopBeforeNewWork, on BOTH arrival paths
//	fresh                -> Fresh
//
// The refusal lives here and not in each caller, so it cannot be skipped by reaching a call by the second path (the #4143 shape).
func (s *execSession) step(ctx context.Context, m api.Module, sp *stepSpec, args stepArgs) int64 {
	c := &stepCtx{ctx: ctx, m: m, s: s, args: args}
	if s.isReplay {
		if s.stepCount < len(s.history) && sp.Role != nil {
			// A multi-record step reads its whole run. It returns unless the history ENDED INSIDE the run, in which case the prefix is consumed
			// and the step falls through to leave replay and run fresh with c.Prefix.
			if res, endedInside := s.replayRun(ctx, m, sp, c); !endedInside {
				return res
			}
		} else if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType != sp.Want {
				// Not consumed: replayDivergence leaves the record for the call it belongs to (cleat#4157).
				return s.replayDivergence(ctx, m, sp.Want, args.outPtr, args.outMaxLen)
			}
			if sp.Identity != nil {
				if why := sp.Identity(args, &rec); why != "" {
					return s.replayIdentityDivergence(ctx, m, sp.Name, why, args.outPtr, args.outMaxLen)
				}
			}
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			return sp.Decode(c, &rec)
		}
		if !sp.StaysInReplayAtFrontier {
			s.leaveReplay(sp)
		}
	}
	if sp.Effect != effectPure {
		refused := false
		if sp.Refuse != nil {
			refused = sp.Refuse(s)
		} else {
			refused = s.stopBeforeNewWork()
		}
		if refused {
			return callSuspendSentinel
		}
	}
	before := s.stepCount
	res := sp.Fresh(c)
	s.checkRecorded(sp, c.recorded, s.stepCount-before)
	return res
}

// leaveReplay is the one place a step leaves replay. Replay never ends with history unread (cleat#4153, #4157): reaching here means the
// session is at or past the end of its history, and a step count that is not exactly the history's length means a step consumed more or
// fewer records than it owned. It is reported and not enforced, because stopping a production run for a bookkeeping disagreement would
// trade a silent bug for a loud outage; the property tests fail on it.
func (s *execSession) leaveReplay(sp *stepSpec) {
	if s.stepCount != len(s.history) {
		s.specViolation(sp, fmt.Sprintf("left replay at step %d with %d records in history", s.stepCount, len(s.history)))
	}
	s.exitReplay()
}

// checkRecorded compares what a step's Fresh recorded with what its spec declared.
func (s *execSession) checkRecorded(sp *stepSpec, recorded, stepsTaken int) {
	ok := true
	switch sp.Records {
	case recordsOne:
		ok = recorded == 1
	case recordsNone:
		ok = recorded == 0
	case recordsMany:
		ok = recorded >= 1
	}
	if !ok {
		s.specViolation(sp, fmt.Sprintf("declares shape %d but recorded %d event(s)", sp.Records, recorded))
	}
	// recordEvent advances stepCount by one; a step that moved it by anything else recorded behind the handle's back.
	if recorded != stepsTaken {
		s.specViolation(sp, fmt.Sprintf("recorded %d event(s) through the handle but the step count moved by %d", recorded, stepsTaken))
	}
}

// specViolation notes a spec that did not do what it declared. It is a defect in the spec, not in the workflow, so it is logged for the
// engine's developers and kept on the session for the tests that assert there are none.
func (s *execSession) specViolation(sp *stepSpec, msg string) {
	full := fmt.Sprintf("step spec %q: %s", sp.Name, msg)
	s.stepViolations = append(s.stepViolations, full)
	if s.engine != nil {
		s.engine.log().ErrorContext(context.Background(), full, "workflow_id", s.workflowID, "step", s.stepCount)
	}
}
