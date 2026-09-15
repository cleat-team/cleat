package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero"
)

// Replay replays a workflow from existing event history. Cached results are
// returned for matching steps; divergence triggers an error.
// queryState contains key-value state set via SetQueryState during execution.
func (e *Engine) Replay(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// Check whether a WasmBackend is registered for this module's language.
	backend, err := e.resolveBackend(wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	if backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, history)
	}

	// Legacy path: compile and replay via the wazero Runtime. The nil check is
	// not defensive padding -- the worker constructs its engines with a nil
	// Runtime, so reaching here without one used to panic rather than report.
	if e.rt == nil {
		return "", nil, nil, nil, nil, fmt.Errorf(
			"host: no runtime available for WASM replay; register a backend for this language with WithBackend")
	}
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// ReplayCompiled is like Replay but takes a pre-compiled module.
// Use this when the module has already been compiled and cached by a
// WorkflowLoader, avoiding redundant compilation.
func (e *Engine) ReplayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, nil)
}

// replayCompiled runs a replay using a pre-compiled module.
func (e *Engine) replayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord, wasmBytes []byte) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	return e.executeCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// advanceReplayStep increments stepCount and invokes the step callback if set.
// rec may be nil for inline replay paths without a full EventRecord.
// Returns false if the callback returned ReplayQuit (caller should abort).
func (s *execSession) advanceReplayStep(ctx context.Context, rec *EventRecord) bool {
	s.stepCount++

	// Track the virtual clock off consumed events, mirroring recordEvent doing
	// it off recorded ones. Without this the two executions are asymmetric: a
	// fresh run's nowMs follows its events, while a replay's stays at the
	// session seed for the whole replay.
	//
	// That asymmetry is observable because the seed and the event timestamps do
	// not come from the same clock. The seed is the workflow row's created_at,
	// written by the database; event timestamps are written by the worker
	// process. Measured 40ms apart with PostgreSQL in a container -- enough
	// that a replay could read a LATER clock than the original execution did,
	// which is a replay divergence and fails any SideEffect that read it.
	if rec != nil && rec.TimestampMs > 0 {
		s.nowMs = rec.TimestampMs
	}

	// Advance the checksum chain over consumed events too.
	//
	// lastChecksum is the predecessor the NEXT recorded event chains from, and
	// nothing seeded it from replayed history: it started empty on every
	// resumed session. So the first event a workflow recorded after a
	// resumption was written with a checksum chained from nothing, while
	// verification recomputes the chain from the events that are actually
	// there -- and the two disagree. The workflow then fails with
	//
	//	checksum mismatch (expected ..., got ...)
	//
	// which reads like corruption and is the integrity mechanism reporting on
	// its own bookkeeping.
	//
	// Found by a workflow that awaits a promise: the await suspends, the
	// resumed execution replays the await from history, finds the promise
	// still pending, and records a second await -- the first new event after a
	// replay, which is exactly the case that was wrong.
	//
	// The fresh paths already do this: recordEvent, callintent, and the atomic
	// child insert in children.go, which had the same omission fixed
	// call-site-by-call-site (#777). This is the same fix in the one place
	// every consumed event passes through, so it does not need repeating for
	// each event type.
	if rec != nil {
		s.lastChecksum = computeEventChecksum(*rec, s.lastChecksum)
	}
	if s.stepCallback == nil {
		return true
	}
	return s.invokeStepCallback(ctx, rec)
}

// invokeStepCallback invokes the step callback if set, building a queryState
// snapshot. Returns false if the callback returned ReplayQuit.
func (s *execSession) invokeStepCallback(ctx context.Context, rec *EventRecord) bool {
	if s.stepCallback == nil {
		return true
	}
	// Snapshot queryState to prevent callback from mutating it.
	qs := make(map[string]string, len(s.queryState))
	for k, v := range s.queryState {
		qs[k] = v
	}
	action := s.stepCallback(s.stepCount-1, rec, qs)
	if action == ReplayQuit {
		if s.stepCancel != nil {
			s.stepCancel()
		}
		return false
	}
	return true
}

// validateReplayStepDensity reports whether the history about to be replayed is
// the one every replay guard assumes: the record at array index i carries
// Step i.
//
// WHY THIS IS A CHECK AND NOT A COMMENT. The invariant is already relied on and
// already written down -- buildFullHistoryFromCompaction reconstructs its
// virtual prefix as `Step: i` and appends the tail with "their Step fields
// already reflect their original positions, which align with the reconstructed
// array indices". Nothing verified it. On the write side it holds by
// construction: fifty sites record `Step: s.stepCount` and recordEvent
// increments that counter, so a run that completes normally produces a dense
// history. It is the READ side, after rows have been through a database, that
// can present something else.
//
// WHAT A VIOLATION MEANS, which is why this is fatal rather than a warning.
// Every guard in this package is written as
//
//	if s.stepCount < len(s.history) { rec := s.history[s.stepCount]; ... }
//
// so a MISSING record does not divert into the type-mismatch branch that
// thirty-one guards have -- it shifts every later record one position earlier.
// The type check then compares the guest's next call against some other step's
// record, and where those happen to agree the guard consumes it and returns
// another step's result. When the shortened history runs out, `stepCount <
// len(history)` is simply false and the session falls through to exitReplay()
// and continues fresh -- the same path a workflow that legitimately reached the
// end of its history takes.
//
// cleat#1429 is NOT an instance of the shape this catches, and saying so is the
// point rather than a caveat. Its repro deletes a sleeping parent's only
// event_history row -- DurableSleep records nothing, so that parent's history is
// the single child_workflow spawn -- which does not perforate the history, it
// EMPTIES it. An empty history is zero iterations here and `isReplay` is false,
// so the run starts fresh and spawns a second child, which is exactly the
// observed symptom and exactly what this cannot see. A peer measured that
// (event_count=1, rows=0 at the moment of truncation) after this check was
// written against the wrong example.
//
// The shape this DOES catch is a perforated history of two or more records: a
// row lost from the front or the middle, leaving the survivors carrying their
// original step numbers.
//
// So the two states the engine cannot otherwise tell apart are "this history
// ended because the run suspended here" and "this history ended early because
// records are missing". Density separates them for every case except a
// truncated TAIL, where the records that would disagree are the ones that are
// gone. That case is not covered here and is not covered anywhere; see the
// issue.
//
// Unconditional, unlike the checksum check beside it, which is gated on
// failOnChecksumMismatch because a mismatch can mean a legacy row format rather
// than a corrupted one. A step gap has no benign reading: the array index and
// the recorded step number are written by the same counter in the same process.
func validateReplayStepDensity(history []EventRecord) error {
	for i := range history {
		if history[i].Step != i {
			return fmt.Errorf(
				"event %d of %d carries step %d: the history is missing at least one record, "+
					"so replay would consume step %d's result where step %d's is expected and "+
					"then re-execute the remainder as though the workflow had reached the end "+
					"of its history",
				i, len(history), history[i].Step, history[i].Step, i)
		}
	}
	return nil
}

// truncatedCompactedEvents returns how many compacted events were dropped to
// keep the compaction-state JSONB bounded, or 0 when none were.
//
// This is the correction term without which reportShortReplayHistory fires on
// every workflow long enough to be truncated -- which is to say, on exactly the
// long-running workflows most likely to be replayed. extractCompactionState
// drops the oldest events (`cs.Events = cs.Events[truncated:]`) and records the
// count here, so the reconstructed history is SHORTER than the instance's
// event_count by design and not by loss.
func truncatedCompactedEvents(cs *CompactionState) int {
	if cs == nil || cs.Summary == nil {
		return 0
	}
	return cs.Summary.TruncatedCount
}

// reportShortReplayHistory reports a replay whose loaded history is shorter
// than the event_count the instance row records. cleat#1507.
//
// WHAT THIS CATCHES THAT validateReplayStepDensity CANNOT. That function walks
// the history asserting history[i].Step == i, so it sees a HOLE. It cannot see
// a missing TAIL: [0,1,2] with step 3 gone satisfies the predicate at every i,
// because there is no index at which to disagree. The instance's own counter is
// the only record of how long the history was supposed to be.
//
// # Why this is one-directional, and why the other direction is NOT a bug
//
// It reports only recorded > loaded. recorded < loaded is the ORDINARY state of
// a crashed segment: the per-step flush persists each event as it happens and
// deliberately does not count it (see TestPerStepFlushDoesNotDoubleCountEvents
// and cleat#1594), while FinalizeWorkflowSegment counts the whole segment when
// it ends. A worker that dies mid-segment therefore leaves rows on disk that
// event_count has never counted -- measured at 5 rows against event_count 0.
// That is precisely the case replay exists to recover, so an equality check
// would fire on every recovery it is meant to protect.
//
// # Why this warns rather than failing the replay
//
// A repeated finalize double-counts: the row insert is idempotent but the
// increment is unconditional, measured as event_count 3 -> 6 over two appends
// of the same three records. That produces this exact signature with no history
// missing. cleat#1507 records that the reachability of that path is NOT
// established -- one caller, no retry, ErrFenceLost returns without
// re-finalizing -- but "I could not construct it" is not "it cannot happen",
// and failing a replay on a signal that might fire on a healthy workflow trades
// a silent wrong answer for a loud one. Promoting this to a hard failure once
// that path is closed is a one-line change; the reverse is not.
func reportShortReplayHistory(recorded, loaded, truncated int) (short bool, missing int) {
	// 0 is also what an unread or failed count looks like -- setup.go swallows
	// GetEventCount's error and leaves the seed at zero -- and a check that
	// cannot tell "no events recorded" from "I did not manage to ask" must not
	// speak.
	//
	// THIS GUARD IS REDUNDANT WITH THE `missing <= 0` BELOW, and saying so is
	// better than implying otherwise: with recorded == 0 and both other terms
	// non-negative, missing is always <= 0, so no input reaches this return
	// that the next one would not also stop. Deleting it leaves every test in
	// a_replay_history_shorter_than_its_record_is_reported_test.go green --
	// measured, not assumed. It stays because it is the guard that survives a
	// change to the comparison: reverting `missing <= 0` to `missing != 0`
	// makes the crashed-segment case fire, and this line is what still holds
	// it. Redundant against today's arithmetic, load-bearing against
	// tomorrow's edit.
	if recorded <= 0 {
		return false, 0
	}
	missing = recorded - (loaded + truncated)
	if missing <= 0 {
		return false, 0
	}
	return true, missing
}

// warnIfReplayHistoryIsShort runs reportShortReplayHistory against this
// engine's persisted count and logs the result. Called from both replay entry
// points, immediately after the density check, so that the history it measures
// is the same one the density check just walked -- post-compaction merge, so
// both sides describe the same events.
func (e *Engine) warnIfReplayHistoryIsShort(ctx context.Context, replayHistory []EventRecord) {
	short, missing := reportShortReplayHistory(
		e.initialEventCount, len(replayHistory), truncatedCompactedEvents(e.compactionState))
	if !short {
		return
	}
	if e.Metrics != nil {
		e.Metrics.RecordReplayShortHistory(ctx)
	}
	e.log().WarnContext(ctx, "replay history is shorter than the instance's recorded event count",
		"workflow_id", e.workflowID, "tenant_id", e.tenantID,
		"recorded_event_count", e.initialEventCount,
		"loaded_events", len(replayHistory),
		"truncated_compacted_events", truncatedCompactedEvents(e.compactionState),
		"missing", missing,
		"note", "a tail may be missing, or a segment's finalize may have counted twice (cleat#1507)")
}
