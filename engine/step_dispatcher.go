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

// resultLayout says how a step's guest decodes the int64 the host returns, which a divergence has to be packed into (cleat#4152). The layouts are
// ABI.md's, and a spec that does not say which is refused at registration: the dispatcher's one divergence answer is the same words in every layout and
// the SAME BITS in none, so a spec that forgot would be answered in whichever layout the reporter happens to write, which a guest on another layout
// reads as a garbage call-error code and a one-byte message (found building the heartbeat call's move, which is the first durable-call-layout spec).
type resultLayout int

const (
	// layoutUnset is the zero value, and is refused by validate(): there is no default a spec may fall into.
	layoutUnset resultLayout = iota
	// layoutSimple: the error code in the low bits and ONE length or value in bits 32-63. Every call whose ABI row reads "upper field 32-63", a call
	// that returns only an error code, and a call that has no output buffer (its divergence is the bare 1).
	layoutSimple
	// layoutDurableCall: responseLen in bits 40-63, callErrorCode in bits 8-39, errCode in bits 0-7 (cleat_call, cleat_call_retry,
	// cleat_call_heartbeat, plugin_call, plugin_call_streaming, cleat_wait_for_event). A divergence carries callErrorUnknown.
	layoutDurableCall
	// layoutAwaitSignals: sigNameLen 48-63, payloadLen 32-47, timedOut 16-31, errCode 0-15 (cleat_await_signals). Its payloadLen is 16 bits and the
	// reporter's length is not: a divergence message goes to the payload buffer and its length is CLAMPED to 65535, so a long message can never spill
	// into the name field. Today's messages are a few hundred bytes, so the clamp is a bound on a layout and not a thing the call does.
	layoutAwaitSignals
)

// stepArgs is the per-call data a spec needs. A spec is a package-level value and holds no closure over the call, so a step costs no
// allocation beyond this struct (section 9, risk 5, of the design); two strings cover the promise calls, and G1b adds the third and fourth that
// signal_workflow and durable_send (three) and fetch (four) need.
type stepArgs struct {
	s1, s2, s3, s4 string
	// i1 is an integer argument: a lock's time to live in milliseconds, or SetScope's previous-scope result, packed (the write into the guest's
	// buffer happens before the step).
	i1 int64
	// i2, i3 and i4 are the rest of a retry policy's integers (cleat#4152, G3): i1 is its maximum attempts, i2 the initial interval in milliseconds, i3 the
	// backoff coefficient times 100 and i4 the maximum interval in milliseconds. A call with fewer integers leaves them zero.
	i2, i3, i4 int64
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
	// spec that failed to do what it declared (for a single-record step as well as a multi-record one: a poll records only when it finds something).
	declined bool
	// declinedWhy is the reason RecordsNothing was given, so a recordsOne spec can be checked against the paths it listed (RecordsNothingOn).
	declinedWhy string
	// unreadAtForeign is how many history records a run left UNREAD when it ended at a foreign record and the spec resumes fresh there (ResumesFreshAtForeign). step reads it
	// to leave replay through the named exit, leaveReplayAtPending, and not through leaveReplay, which treats unread history as a violation.
	unreadAtForeign int
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
	c.declined = true
	c.declinedWhy = why
}

// RecordPersisted is Record for an event that the STORE has already made durable and that therefore must not be flushed again: the write-ahead path
// of a call declared WriteAheadIntent commits its record itself (callintent.go: freshCallWithIntent), computes the checksum from it, and then needs the
// session's history, step count and checksum chain to catch up without a second write (recordEventPersisted). That path never went through recordEvent
// and so never through Record, which is what makes it invisible to the shape check: it moves the step count by one and the handle's count by none, and
// the second is what the first is compared with. Going through here counts the event, so a spec that records this way is held to the shape it declared
// like every other. A spec that calls recordEventPersisted directly instead is reported, and a test says so (cleat#4152, G3, DurableCall).
func (c *stepCtx) RecordPersisted(rec EventRecord, checksum string) {
	c.recorded++
	c.s.recordEventPersisted(rec, checksum)
}

// ContinueAsNewAtEventCap starts the continue-as-new that --max-quota-events asks for, from inside a spec's Fresh, and counts the event it records
// (cleat#4152, G3). The cap is the one place a Fresh records an event on its own behalf through a CALL: triggerEventCapContinueAsNew runs ContinueAsNew,
// which records a continue_as_new event at the step this call was on and sets the suspension. That event went through recordEvent and not through Record,
// so the shape check read the step count moving by one and the handle's count by none, and reported it. Measured on develop 615c81c4: await_promise and
// await_signals each logged `recorded 0 event(s) through the handle but the step count moved by 1` every time the cap fired, at error level, and no test
// read it. Counting it here is a named method and not a wrapper that takes any function, because a wrapper would be a way to record behind the handle and
// have the count agree: this one counts what the continue-as-new did, and only it.
func (c *stepCtx) ContinueAsNewAtEventCap() {
	before := c.s.stepCount
	c.s.triggerEventCapContinueAsNew(c.ctx, c.m)
	c.recorded += c.s.stepCount - before
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
	// Result is the layout the call's guest decodes (see resultLayout). REQUIRED: validate() refuses layoutUnset, the way it refuses a spec with
	// neither Identity nor NoIdentity, so a call added later cannot forget to say.
	Result resultLayout
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
	// ResumesFreshAtForeign, for a MULTI-RECORD step (Role set, Identity required, and not EndsWithoutFinal), makes "prefix records, then a FOREIGN record" end the run the
	// way "prefix records, then the end of history" does: the prefix records are consumed, replay is left at the foreign record, and Fresh continues with c.Prefix
	// (cleat#4152, W3b: await_any_child, which records an empty await per wake and answers at a later step, and re-checks its children when it finds only the empties).
	// Where EndsWithoutFinal says such a run is COMPLETE and DecodeRun answers it, this says the call is not finished: it goes on, fresh.
	//
	// WHAT IS LEFT UNREAD, AS FOR OverPending (cleat#4285). The foreign record is history the call leaves UNREAD. When it is a call the guest's BODY recorded after
	// the empties, replayRun refuses it as a divergence before anything is consumed (staleCallAfterPending); what is still left unread, reported with a count, is a
	// defer drain's record, a record expanded from a compaction and a record that is no call's effect.
	ResumesFreshAtForeign bool
	// ForeignFirst answers a call whose FIRST record belongs to another call (a record of another TYPE for a single-record step, a record whose Role is
	// foreign for a multi-record one), WITHOUT consuming it and without leaving replay, instead of reporting a divergence. ok false means "not
	// answerable", and the divergence follows as usual. A SINGLE-record step uses it for a poll (cleat#4152, W1: durable_poll_update): the guest polls
	// at every suspension and most polls find nothing, so a record that is another call's means "nothing was delivered here on the original run", and ending
	// replay or reporting a divergence at the first such poll would be wrong. For a multi-record step it exists for one case (cleat#4152, W5): a history written before cleat#4151 recorded nothing for a no-wait poll that timed out, so the record at the
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
	// WHAT IS LEFT UNREAD, AND WHAT IS NOT (cleat#4285). The pending record is the history's last event when a run is resumed. When it is NOT, the
	// records after it are left unread when replay leaves here. A call the guest's BODY recorded there is the case that ran twice (a guest that ignored the
	// suspend word went on to make it): step refuses that as a divergence that consumes nothing (staleCallAfterPending; the message says what to do). What is
	// still left unread, on purpose, is what is not that: a defer drain's own records (cleat#3429), a record expanded from a compaction (whose stamps are
	// unknown), and a record that is no call's effect. Those are reported (a warning with the count) and named in
	// a_host_call_never_leaves_replay_with_history_unread_test.go, whose entry probes both halves.
	//
	// WHAT A PENDING MARKER RESTS ON, a store invariant and not a rule of this field: the two child awaits tell "pending" from "answered" by an EMPTY
	// answer, so a finished child's answer must never be empty. coerceResultJSON stores an empty result as {} and nonEmptyChildError keeps a failure's
	// message non-empty; without either, a finished child would read as still pending and be answered over (noted by the reviewer of #4290).
	OverPending func(a stepArgs, rec *EventRecord) bool
	// AfterSuspend is what the step answers, WITHOUT running or recording anything, when this execution has already suspended (cleat#4285): step
	// applies that refusal to every non-Pure step after its other refusals, and this says which word. nil answers callSuspendSentinel, the stop word every
	// stop site's SDK decoders test first. A spec whose call is not a stop site, so that its decoders do not know that word, answers what they do: the
	// child awaits their own suspension word (1<<62), and list_crons and release_lock an ordinary error (callRefusedAfterSuspend), which keeps them
	// non-sites. Not allowed on a Pure step, which is never refused.
	AfterSuspend func(c *stepCtx) int64
	// DivergenceDetail adds what the call ASKED and what was RECORDED to the divergence message the dispatcher reports for it (cleat#4152, G3). The old per-call messages
	// printed the request (`  actual request: ...` / `  expected request: ...`), a run's IDs, a Fetch's URL and bodies, a schedule's four arguments; the shared
	// message names only the step and both event types, and the moves that went onto it lost the rest silently, including text docs/troubleshooting.md shows to
	// users and TestEngineReplayDivergence pins. a is the call's arguments and rec the record that diverged (a record of the wrong TYPE or one that failed Identity);
	// it returns "" or text that starts with a newline and indents each line by two spaces, and it must not touch the world. Use truncateWithHash on any body, as
	// the old messages did. Exactly one of this and NoDivergenceDetail is set (validate()), so a call that has detail cannot lose it by being moved without saying so.
	DivergenceDetail func(a stepArgs, rec *EventRecord) string
	// NoDivergenceDetail says why a call has nothing to add to a divergence message, when it has nothing: the reason is for the reader of the spec and must say what the
	// call's old message carried, so a move that dropped detail cannot hide behind it (cleat#4152, G3).
	NoDivergenceDetail string
	// RecordsNothingOn, for a spec that declares recordsOne, lists the paths on which its Fresh deliberately records NOTHING, each by the reason it gives
	// to c.RecordsNothing (cleat#4152, G3: DurableCall answers a cancelled workflow, a hard stop, a worker presumed lost and the event cap without
	// recording an event, and says so). It is OPT-IN, and the default is the rejection that was always there: a recordsOne spec must record exactly one
	// event, so a Fresh that returns without recording is reported as a spec that did not do what it declared. Listing a path does not loosen that for
	// any other path of the same spec: a Fresh that records nothing for a reason it did not list, or says nothing, is still reported. Needs recordsOne
	// (a recordsMany spec already may record nothing and say so, and a recordsNone one never records), and an entry may not be empty or repeated.
	RecordsNothingOn []string
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
	case sp.Result == layoutUnset:
		return fmt.Errorf("step spec %q declares no result layout (Result is required): a divergence has to be packed in the layout its guest decodes, and there is no default", sp.Name)
	case sp.Result < layoutSimple || sp.Result > layoutAwaitSignals:
		return fmt.Errorf("step spec %q declares a result layout that does not exist (Result is %d): a divergence has to be packed in the layout its guest decodes", sp.Name, sp.Result)
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
	case sp.AfterSuspend != nil && sp.Effect == effectPure:
		return fmt.Errorf("step spec %q is Pure and has an AfterSuspend: a Pure step is never refused", sp.Name)
	case sp.OverPending != nil && (sp.Role != nil || sp.Records == recordsNone):
		return fmt.Errorf("step spec %q has an OverPending but is a multi-record step or records nothing: OverPending is a single-record step whose answer is recorded over its own pending record", sp.Name)
	case sp.ResumesFreshAtForeign && (sp.Role == nil || sp.EndsWithoutFinal || sp.Identity == nil):
		return fmt.Errorf("step spec %q has ResumesFreshAtForeign but is not a multi-record step with an Identity, or also declares EndsWithoutFinal", sp.Name)
	case sp.MaxPrefix < 0 || (sp.MaxPrefix > 0 && sp.Role == nil):
		return fmt.Errorf("step spec %q has MaxPrefix %d: it bounds the prefix records of a run, so it needs a Role and cannot be negative", sp.Name, sp.MaxPrefix)
	case sp.EndsWithoutFinal && sp.Identity == nil:
		return fmt.Errorf("step spec %q has EndsWithoutFinal but no Identity: a run that can end without a final is bounded by Identity, and without one it cannot tell its own records from the next call's", sp.Name)
	case (sp.DivergenceDetail == nil) == (sp.NoDivergenceDetail == ""):
		return fmt.Errorf("step spec %q must set exactly one of DivergenceDetail and NoDivergenceDetail: a call either says what it asked and what was recorded when it diverges, or says why its old message carried nothing beyond the step and the event types (cleat#4152, G3)", sp.Name)
	case (sp.Identity == nil) == (sp.NoIdentity == ""):
		return fmt.Errorf("step spec %q must set exactly one of Identity and NoIdentity: a call either names what identifies it or says why it has nothing to compare", sp.Name)
	case sp.Refuse != nil && sp.Effect == effectPure:
		return fmt.Errorf("step spec %q is Pure and has a Refuse: a Pure step is never refused", sp.Name)
	case sp.StaysInReplayAtFrontier && (sp.Records != recordsNone || sp.Effect == effectPure):
		return fmt.Errorf("step spec %q stays in replay at the frontier but records or is Pure: only a non-Pure step that records nothing may answer without leaving replay", sp.Name)
	case len(sp.RecordsNothingOn) > 0 && sp.Records != recordsOne:
		return fmt.Errorf("step spec %q lists RecordsNothingOn but does not declare recordsOne: only a spec that must record exactly one event needs to name the paths on which it records none", sp.Name)
	case sp.Effect == effectPure && sp.Records != recordsNone:
		return fmt.Errorf("step spec %q is Pure and declares it records events: a Pure step is not refused in a segment, so it must record nothing", sp.Name)
	}
	seen := map[string]bool{}
	for _, why := range sp.RecordsNothingOn {
		if why == "" || seen[why] {
			return fmt.Errorf("step spec %q has an empty or repeated RecordsNothingOn entry %q: each path is named once, by the reason its Fresh gives to RecordsNothing", sp.Name, why)
		}
		seen[why] = true
	}
	return nil
}

// divergenceSweepNote is the evidence behind a NoDivergenceDetail reason: every engine commit since the dispatcher began that removed a divergence or mismatch message
// was read (cleat#4152, G3), and none removed more than the reason says for the call. A reason that says otherwise is wrong and the sweep is the thing to re-run.
const divergenceSweepNote = "checked by reading every engine commit since the dispatcher began that removed a divergence or mismatch message (cleat#4152, G3)"

// divergenceDetailFor is the spec's detail for the record that diverged, or "" when it has none.
func (sp *stepSpec) divergenceDetailFor(a stepArgs, rec *EventRecord) string {
	if sp.DivergenceDetail == nil {
		return ""
	}
	return sp.DivergenceDetail(a, rec)
}

// divergenceAnswer puts the reporter's answer into the layout the spec's guest decodes. The reporter's is the simple layout: error code 1 in the low
// bits and the message length in bits 32-63. A simple spec takes it as it is; the others re-pack the same two numbers.
func (sp *stepSpec) divergenceAnswer(res int64) int64 {
	written := uint64(res) >> 32
	switch sp.Result {
	case layoutDurableCall:
		return packDurableCallResult(int(written), callErrorUnknown, byte(res))
	case layoutAwaitSignals:
		if written > 0xFFFF {
			written = 0xFFFF // payloadLen is 16 bits; a longer length would spill into sigNameLen
		}
		return int64(uint64(byte(res)) | written<<32)
	}
	return res
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
				// A poll answers "nothing here" and leaves the record, and replay, as they were (ForeignFirst).
				if sp.ForeignFirst != nil {
					if res, ok := sp.ForeignFirst(args); ok {
						return res
					}
				}
				// Not consumed: replayDivergence leaves the record for the call it belongs to (cleat#4157).
				return sp.divergenceAnswer(s.replayDivergenceAt(ctx, m, &rec, sp.Want, sp.divergenceDetailFor(args, &rec), args.outPtr, args.outMaxLen))
			}
			if sp.Identity != nil {
				if why := sp.Identity(args, &rec); why != "" {
					return sp.divergenceAnswer(s.replayIdentityDivergenceAt(ctx, m, &rec, sp.Name, why, sp.divergenceDetailFor(args, &rec), args.outPtr, args.outMaxLen))
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
			if later := s.staleCallAfterPending(s.stepCount + 1); later >= 0 {
				return s.staleCallDivergence(ctx, m, sp, s.stepCount, later, args.outPtr, args.outMaxLen)
			}
			s.leaveReplayAtPending(sp, len(s.history)-s.stepCount-1)
		} else if c.unreadAtForeign > 0 {
			s.leaveReplayAtPending(sp, c.unreadAtForeign)
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
		// A session that has already suspended starts no fresh work, whatever else would have allowed it (cleat#4285).
		if res, ok := s.refuseIfSuspendedWith(sp.Name, func() int64 {
			if sp.AfterSuspend != nil {
				return sp.AfterSuspend(c)
			}
			return callSuspendSentinel
		}); ok {
			return res
		}
	}
	before := s.stepCount
	res := sp.Fresh(c)
	s.checkRecorded(sp, c.recorded, s.stepCount-before, len(c.Prefix) > 0, res == callSuspendSentinel || c.declined, c.declined, c.declinedWhy)
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
// so the history is read to its last record when the pending one is last, which is the ordinary resume. Records after it are left unread. When one of them
// records a call's EFFECT (staleCallAfterPending) the step refuses before it gets here: that call would run twice (cleat#4285). What reaches here is the
// rest: a drain's records, a record whose stamp compaction dropped, and a record that is no effect, which are reported and not enforced because stopping a
// production run for them would trade a quiet, harmless re-read for a loud outage.
func (s *execSession) leaveReplayAtPending(sp *stepSpec, unread int) {
	if unread > 0 && s.engine != nil {
		s.engine.log().WarnContext(context.Background(), "step left replay with later records unread, after its own pending record or at the end of its run: a call recorded after them will run again (cleat#4285)",
			"workflow_id", s.workflowID, "step", sp.Name, "at", s.stepCount, "unread", unread)
	}
	s.exitReplay()
}

// staleCallAfterPending returns the index of the first record at or after `from` that a call made by the guest's BODY left, after a pending await the guest had
// been told to stop at, or -1. Such a record means the guest ignored a suspension (cleat#4285): replaying PAST it would leave it unread and run the call again.
//
// A record is one only if ALL of these hold, and each exemption is a way a healthy history reaches this shape:
//   - it is the effect of a call (isStoppableEvent, the engine's own list): an admin audit row or an update delivery can sit after a pending await in a
//     healthy history, and re-reading them runs nothing twice;
//   - it carries neither drain stamp (InDeferPhase, InDeferSegment): a defer body that awaits a child during the guest's own drain records its later
//     defers after the pending await by design (cleat#3429), the same exemption the host applies when it refuses (guestDrainIsExempt);
//   - it was read from an event row and not expanded from a compaction (compactedRecords): a record below that index may have an UNKNOWN stamp, because a state
//     compacted by a build before cleat#4310 did not carry the stamps and cannot be told apart from one compacted after, and treating unknown as absent would
//     fail a healthy workflow. The exemption is deliberately still wide: it covers records compacted after cleat#4310 too, which do carry them, until a follow-up
//     can tell the two kinds of state apart.
func (s *execSession) staleCallAfterPending(from int) int {
	for i := max(from, s.compactedRecords); i < len(s.history); i++ {
		rec := s.history[i]
		if rec.InDeferPhase || rec.InDeferSegment || !isStoppableEvent(rec) {
			continue
		}
		return i
	}
	return -1
}

// staleCallDivergence is the divergence for a pending await followed by a call it did not stop the guest from making (cleat#4285). It consumes nothing, and its
// text names the cause and the way out, because the operator who reads it did nothing wrong and needs to know what to do.
func (s *execSession) staleCallDivergence(ctx context.Context, m api.Module, sp *stepSpec, pending, later int, outPtr, outMaxLen uint32) int64 {
	p, l := &s.history[pending], &s.history[later]
	return sp.divergenceAnswer(s.reportReplayDivergence(ctx, m, fmt.Sprintf(
		"replay divergence at step %d: %s found its own pending %s record followed by a %s recorded at step %d. "+
			"This history was written by a guest that ignored a child-await suspension before 0.6.0 (cleat#4285); replaying it would run that call a second time. "+
			"Terminate or reset the workflow.",
		p.Step, sp.Name, p.EventType, l.EventType, l.Step), outPtr, outMaxLen))
}

// checkRecorded compares what a step's Fresh recorded with what its spec declared.
func (s *execSession) checkRecorded(sp *stepSpec, recorded, stepsTaken int, continuedRun, refused, declined bool, declinedWhy string) {
	ok := true
	switch sp.Records {
	case recordsOne:
		// Exactly one, with ONE exception that a spec must ask for: a path it listed in RecordsNothingOn, taken and announced through RecordsNothing under
		// that name. `refused` is deliberately not consulted here (it is for recordsMany): returning the suspend sentinel does not excuse a recordsOne spec.
		ok = recorded == 1 || (recorded == 0 && declined && sp.listsNothingPath(declinedWhy))
		if recorded == 0 && declined && !ok {
			s.specViolation(sp, fmt.Sprintf("records nothing on a path (%q) that its RecordsNothingOn does not list", declinedWhy))
		}
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

// listsNothingPath says whether the spec named this reason among the paths on which it records nothing.
func (sp *stepSpec) listsNothingPath(why string) bool {
	for _, have := range sp.RecordsNothingOn {
		if have == why {
			return true
		}
	}
	return false
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
