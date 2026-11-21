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
	// effectEffect: reaches the outside world or changes durable state. Refused when stopBeforeNewWork: in a segment's body, after a worker
	// shutdown, and after an event that could not be written.
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
// allocation beyond this struct (section 9, risk 5, of the design); two strings cover the promise calls, and G1b adds the third and fourth that
// signal_workflow and durable_send (three) and fetch (four) need.
type stepArgs struct {
	s1, s2, s3, s4 string
	// i1 is an integer argument: a lock's time to live in milliseconds, or SetScope's previous-scope result, packed (the write into the guest's
	// buffer happens before the step).
	i1 int64
	// outPtr and outMaxLen are where a divergence message is written, and where the call's own output goes; zero when it has none.
	outPtr, outMaxLen uint32
	// out2Ptr and out2MaxLen are a second buffer for a call that returns two things (an await's delivery: the signal's name and its payload).
	out2Ptr, out2MaxLen uint32
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
	// declined is set by RecordsNothing: Fresh took a path that records nothing on purpose and says why, so the shape check does not read it as a
	// spec that failed to do what it declared.
	declined bool
}

// Record records an event where the call records it today. It is the only way a spec's Fresh writes history, which is what lets the
// dispatcher check afterwards that it wrote what the spec declared. Calls differ on WHEN they record (a promise create records first and
// writes its store after), so Fresh calls this where its call did and the dispatcher imposes no order.
func (c *stepCtx) Record(rec EventRecord) eventPersistence {
	c.recorded++
	return c.s.recordEvent(rec)
}

// RecordsNothing says that this Fresh took a path that records nothing on purpose, and why (the reason is for the reader of the spec; the
// dispatcher only needs the fact). Without it a multi-record step that returned an error word and recorded nothing is indistinguishable from one
// that forgot to record: the only other way to record nothing is to refuse with the suspend sentinel. await_promise's no-store answer is the case
// (it must stay an error word with an error code, which a replay and the guest read).
func (c *stepCtx) RecordsNothing(why string) {
	_ = why
	c.declined = true
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
	// pending record then its resolution). nil means a single-record step. Role classifies a record by its type and shape; WHAT it
	// names (the key, the service and op) is Identity's, applied to every record of the run. A value other than the three roles is a bug in
	// the spec and is always a divergence. See replayRun for what each ending means, and EndsWithoutFinal for the one case where Identity also
	// decides where a run ENDS.
	Role func(a stepArgs, rec *EventRecord) recordRole
	// DecodeRun turns a complete run into the call's result: the records in order, the last of them the roleFinal one, or, for a spec with
	// EndsWithoutFinal, prefix records only. It may keep a replay-time write its call made today (a pending call's ambiguity record) and
	// the spec's comment says so; otherwise it must not touch the world.
	DecodeRun func(c *stepCtx, run []EventRecord) int64
	// EndsWithoutFinal says a run of prefix records followed by a FOREIGN record is a complete run with no final: an await that was recorded
	// and then not answered (it timed out). Without it that shape is a divergence, as for a retried call or SetScope.
	//
	// A run that can end without its final cannot be bounded by record TYPE, because the next call's records have the same types: two awaits
	// in a row, each never answered, are [pending a, pending b, ...], and step a's run would swallow b's record. So for such a spec Identity
	// bounds the run: a record that fails Identity after at least one prefix record is the next call's and ends this run. The spec must
	// therefore have an Identity (validate() refuses EndsWithoutFinal with NoIdentity), and its Identity must tell this call's records from the
	// next call's. (Measured by cleat-review3 on the first version, with two such awaits: a divergence with an Identity, and b's record
	// consumed by a's step without one.)
	EndsWithoutFinal bool
	// MaxPrefix, when above zero, is the most prefix records one run may hold: a prefix record past it is FOREIGN and ends the run (cleat#4152,
	// W5). An await's run holds at most one, and Identity cannot say so: a polling loop that times out records [await, await, ...] with the
	// SAME names each time, so by type and identity alone the first call's run swallows the second call's record, and the pair of them reads as
	// one delivery (measured on the old code by a_await_signals_behaves_as_recorded_before_the_dispatcher_test.go: the first call times out and
	// the second one pairs). 0 means unlimited, which is a retried call's or SetScope's shape. Needs a Role.
	MaxPrefix int
	// ForeignFirst, for a multi-record step, answers a call whose FIRST record belongs to another call, WITHOUT consuming it and without leaving
	// replay, instead of reporting a divergence. ok false means "not answerable", and the divergence follows as usual. It exists for one
	// case (cleat#4152, W5): a history written before cleat#4151 recorded nothing for a no-wait poll that timed out, so the record at the
	// poll's step is the NEXT call's, and the poll's answer was always the timeout (had a signal been found it would have been a record). Dropping
	// that would turn a resumable history into a divergence, which a migration may not do. It is not called for a record whose Role is not
	// one of the three (a bug in the spec is always a divergence), and it must not touch the world: it is a pure function of the arguments.
	ForeignFirst func(a stepArgs) (res int64, ok bool)
	// OverPending, for a SINGLE-record step, names a record of this call that is a PENDING MARKER: the call was recorded and not yet answered, and its
	// answer is recorded OVER that record, at the same step, by the next wake (cleat#4152, W3: await_child and await_all_children record an empty
	// await, and the store's conflict clause reopens it). Such a record is not consumed: step leaves replay AT it, calls Fresh with stepCount still on it,
	// and Fresh records the answer at that step, so the step count moves by one for a record that was already there once. It is checked after the type
	// and Identity, so a pending record naming ANOTHER call is a divergence, not this call's to answer over.
	//
	// THE EXEMPTION THIS CARRIES, AND IT IS A DEFECT KEPT ON PURPOSE (cleat#4285). The pending record is the history's last event when a run is
	// resumed. When it is NOT, because a guest that ignored the suspend word went on to make a call, that call's record follows it, and leaving
	// replay here leaves it unread, so the call runs again. The old code did exactly that and this keeps it, so a move changes nothing; it is
	// reported (a warning with the count) and named in a_host_call_never_leaves_replay_with_history_unread_test.go, whose entry fails the moment the
	// behaviour changes. It becomes a divergence when #4285's host-side guard and the Java fix have landed, and not before.
	//
	// WHAT A PENDING MARKER RESTS ON, a store invariant and not a rule of this field: the two child awaits tell "pending" from "answered" by an EMPTY
	// answer, so a finished child's answer must never be empty. coerceResultJSON stores an empty result as {} and nonEmptyChildError keeps a failure's
	// message non-empty; without either, a finished child would read as still pending and be answered over (noted by the reviewer of #4290).
	OverPending func(a stepArgs, rec *EventRecord) bool
	// Fresh does the work and records, through c.Record, where the call records. It is called only once the dispatcher has decided
	// fresh work is allowed.
	Fresh func(c *stepCtx) int64
}

// stepSpecs is the registry of every spec in the engine, filled by registerStepSpec: each spec registers itself in an init() beside its own declaration,
// so a host call moved onto step adds its spec to the registry by being written, in the file it lives in, and not by an edit to a list every move touched
// (which three open PRs conflicted on, one after another, in one week of 2026-11). It exists for the dispatcher's tests (step_dispatcher_test.go), which
// run each property over every spec on it, and for the check below that every declaration is registered.
var stepSpecs []*stepSpec

// registerStepSpec adds a spec to the registry. A malformed spec is a programming error, so it stops the process at start-up, as a bad regexp does, and
// not at the first workflow that reaches the call: it is validated HERE, once, for every spec, which is what the old start-up loop did for the list. Two
// specs may not share a name or a pointer: a duplicate is a spec registered twice, or two calls that would be indistinguishable in a divergence message.
func registerStepSpec(sp *stepSpec) { registerStepSpecInto(&stepSpecs, sp) }

// registerStepSpecInto is registerStepSpec over a list the caller names, so the rules can be tested without touching the real registry.
func registerStepSpecInto(list *[]*stepSpec, sp *stepSpec) {
	if err := sp.validate(); err != nil {
		panic(err)
	}
	for _, have := range *list {
		switch {
		case have == sp:
			panic(fmt.Errorf("step spec %q is registered twice", sp.Name))
		case have.Name == sp.Name:
			panic(fmt.Errorf("two step specs are named %q: a divergence could not say which call it was", sp.Name))
		}
	}
	*list = append(*list, sp)
}

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
	case sp.OverPending != nil && (sp.Role != nil || sp.Records == recordsNone):
		return fmt.Errorf("step spec %q has an OverPending but is a multi-record step or records nothing: OverPending is a single-record step whose answer is recorded over its own pending record", sp.Name)
	case sp.ForeignFirst != nil && sp.Role == nil:
		return fmt.Errorf("step spec %q has a ForeignFirst but no Role: it answers a multi-record step whose first record is another call's", sp.Name)
	case sp.MaxPrefix < 0 || (sp.MaxPrefix > 0 && sp.Role == nil):
		return fmt.Errorf("step spec %q has MaxPrefix %d: it bounds the prefix records of a run, so it needs a Role and cannot be negative", sp.Name, sp.MaxPrefix)
	case sp.EndsWithoutFinal && sp.Identity == nil:
		return fmt.Errorf("step spec %q has EndsWithoutFinal but no Identity: a run that can end without a final is bounded by Identity, and without one it cannot tell its own records from the next call's", sp.Name)
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

// neverRefuse is a Refuse for a step that is NEVER refused: not in a segment's body, not after a worker shutdown and not after an event that could
// not be written. It has two uses, with two different reasons, and in both the reason is a measurement of what the call did before it moved and not
// an argument.
//
//   - ReleaseLock, which gives back something the workflow holds. AcquireLock is refused in all four states and ReleaseLock in none
//     (a_lock_call_in_each_stop_state_test.go, taken before the move), and a defer draining on a shutting-down worker is the case that needs it: it
//     cannot take a lock but must be able to give one back, or the lock stays held until its lease runs out. The design's blanket rule was argued
//     from a segment's body alone, which is only one of the three states stopBeforeNewWork covers. Whether a segment's BODY should release is a
//     separate decision.
//   - ListCrons, a read that records its answer. It listed in all four states before it moved (the golden table in
//     a_cron_single_steps_behave_as_recorded_before_the_dispatcher_test.go, rows segment_*, write_failed_* and shutdown_fresh), and this keeps that.
//     Here the design's rule WOULD refuse it, and whether it should is cleat#4239; unlike the lock, nothing shows a defer that needs it unrefused.
//
// Both keep what the call did before it moved. The scans that count host stop sites (stop_correspondence_guard_test.go, java_sdk_stop_bit_parity_test.go) read the spec
// declaration for exactly `Refuse: neverRefuse` and do not count such a call; any other Refuse counts as a site, because they cannot prove it
// never refuses and an over-count fails loudly where an under-count would pass a call the SDKs do not guard.
func neverRefuse(*execSession) bool { return false }

// step runs one host call as a step.
//
//	replay, at a record  -> the record must be Want, else a divergence; consume it; Decode
//	replay, at the end   -> leave replay (asserting the history was all read), then as fresh
//	any non-Pure step    -> refused (callSuspendSentinel) when its Refuse says so (nil: stopBeforeNewWork), on BOTH arrival paths
//	fresh                -> Fresh
//
// The refusal lives here and not in each caller, so it cannot be skipped by reaching a call by the second path (the #4143 shape).
func (s *execSession) step(ctx context.Context, m api.Module, sp *stepSpec, args stepArgs) int64 {
	c := &stepCtx{ctx: ctx, m: m, s: s, args: args}
	overPending := false
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
			if sp.OverPending != nil && sp.OverPending(args, &rec) {
				// Its own pending record: not consumed. The answer is recorded over it, so the step leaves replay with stepCount ON it.
				overPending = true
			} else {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return sp.Decode(c, &rec)
			}
		}
		if overPending {
			s.leaveReplayAtPending(sp)
		} else if !sp.StaysInReplayAtFrontier {
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
	s.checkRecorded(sp, c.recorded, s.stepCount-before, len(c.Prefix) > 0, res == callSuspendSentinel || c.declined)
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

// leaveReplayAtPending is leaveReplay for a step that found its OWN pending record and does not consume it (OverPending): stepCount is ON that record,
// so the history is read to its last record when the pending one is last, which is the ordinary resume. Records after it are left unread, which is the
// kept defect of cleat#4285 (see OverPending): it is reported and not enforced, like every disagreement of this kind, because stopping a production run
// for it would trade a silent re-run for a loud outage.
func (s *execSession) leaveReplayAtPending(sp *stepSpec) {
	if unread := len(s.history) - s.stepCount - 1; unread > 0 && s.engine != nil {
		s.engine.log().WarnContext(context.Background(), "step left replay at its own pending record with later records unread: a call recorded after it will run again (cleat#4285)",
			"workflow_id", s.workflowID, "step", sp.Name, "at", s.stepCount, "unread", unread)
	}
	s.exitReplay()
}

// checkRecorded compares what a step's Fresh recorded with what its spec declared.
func (s *execSession) checkRecorded(sp *stepSpec, recorded, stepsTaken int, continuedRun, refused bool) {
	ok := true
	switch sp.Records {
	case recordsOne:
		ok = recorded == 1
	case recordsNone:
		ok = recorded == 0
	case recordsMany:
		// A step that CONTINUED a run its history ended inside (c.Prefix) may record nothing new: an await whose record is already there and
		// that suspends again, or times out at its recorded deadline, has written nothing this time. A Fresh that REFUSED (it returned the
		// suspend sentinel) records nothing by design: an await's second-await guard and its event-cap stop are refusals inside Fresh.
		ok = recorded >= 1 || continuedRun || refused
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
