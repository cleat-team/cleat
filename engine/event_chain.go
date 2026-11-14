package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// Event checksums chain: each row's checksum covers the row and the checksum of
// its predecessor (computeEventChecksum). Compaction deletes the leading rows
// and keeps the rest, so the first row it keeps chains from a row that no longer
// exists. cleat#3837: verification and replay both started that row from an
// empty predecessor, so every compacted run failed verification on resume.
//
// The predecessor's stored checksum is therefore recorded in the compaction
// state, at the moment the row is deleted, and both consumers start the chain
// from it. The checksum is xxHash64, unkeyed, so this detects corruption and
// says nothing about tampering: a writer who can edit a row can recompute its
// checksum, and the state it is anchored to.

// firstRetainedStep returns the step of the first event row a compaction left in
// the database: the step the tail starts at, which is not CompactedStep when the
// state was truncated (extractCompactionState subtracts the truncated count from
// CompactedStep and records it in Summary).
func (cs *CompactionState) firstRetainedStep() int {
	return cs.CompactedStep + truncatedCompactedEvents(cs)
}

// withBoundaryChecksum returns compactionState, a marshalled CompactionState,
// with PrevChecksum set. It edits the JSON rather than the struct so a field the
// caller's version of the struct does not know about is kept.
func withBoundaryChecksum(compactionState []byte, checksum string) ([]byte, error) {
	if checksum == "" {
		return compactionState, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(compactionState, &m); err != nil {
		return nil, fmt.Errorf("compaction state: %w", err)
	}
	quoted, err := json.Marshal(checksum)
	if err != nil {
		return nil, err
	}
	m["prev_checksum"] = quoted
	return json.Marshal(m)
}

// legacyAnchorWarned bounds the "verified against a legacy compaction state"
// warning to one per run. A run is verified at every resume, and a long-lived one
// resumes thousands of times.
var legacyAnchorWarned sync.Map

func warnLegacyAnchorOnce(workflowID string, step int) {
	if _, loaded := legacyAnchorWarned.LoadOrStore(workflowID, struct{}{}); loaded {
		return
	}
	slog.Warn("compaction state carries no boundary checksum (compacted before cleat#3837); "+
		"the first retained event is trusted as the start of its chain, so a change to that one row is not detectable",
		"workflow_id", workflowID, "step", step)
}

// verifyEventChain recomputes the checksum chain over events and compares it with
// stored, the checksums read from the database, keyed by step. cs is the run's
// compaction state, or nil.
//
// Without a compaction state the chain starts empty, exactly as it always did.
// With one:
//
//   - events[0] must be the first retained step. A history that begins anywhere
//     else lost rows the state does not account for, and fails closed.
//   - If the state carries PrevChecksum, events[0] is verified against it like any
//     other row.
//   - If it does not (compacted before cleat#3837), events[0]'s stored checksum is
//     taken as the start of the chain and only the rows after it are verified.
//     That is a hole of exactly one row, and it is the only way a run compacted by
//     an earlier release can resume at all.
func verifyEventChain(workflowID string, events []EventRecord, stored map[int]string, cs *CompactionState) error {
	var prevChecksum string
	for i, ev := range events {
		expected, ok := stored[ev.Step]

		if i == 0 && cs != nil {
			if ev.Step != cs.firstRetainedStep() {
				return fmt.Errorf("verify events: workflow %s: history starts at step %d but its compaction state "+
					"says the first retained step is %d", workflowID, ev.Step, cs.firstRetainedStep())
			}
			if cs.PrevChecksum == "" {
				warnLegacyAnchorOnce(workflowID, ev.Step)
				if ok && expected != "" {
					prevChecksum = expected
				}
				continue
			}
			prevChecksum = cs.PrevChecksum
		}

		if !ok || expected == "" {
			prevChecksum = "" // Missing event breaks the chain
			continue          // No stored checksum for this step (pre-migration partial data).
		}
		actual := computeEventChecksum(ev, prevChecksum)
		if actual != expected {
			return fmt.Errorf("verify events: workflow %s step %d: checksum mismatch (expected %s, got %s)",
				workflowID, ev.Step, expected, actual)
		}
		prevChecksum = expected
	}
	return nil
}

// chainAnchor tells a session where the stored checksum chain resumes after a
// compaction. See advanceReplayStep.
type chainAnchor struct {
	// firstStep is the step of the first retained row. Records before it are the
	// virtual prefix rebuilt from the compaction state: they are not rows, and
	// they carry no checksum.
	firstStep int
	// prev is the checksum the first retained row chains from. Empty for a state
	// compacted before cleat#3837.
	prev string
	// firstChecksum is the stored checksum of the first retained row, supplied by
	// the worker for a state with no prev. Empty otherwise.
	firstChecksum string
}

func (e *Engine) chainAnchor() *chainAnchor {
	if e.compactionState == nil {
		return nil
	}
	return &chainAnchor{
		firstStep:     e.compactionState.firstRetainedStep(),
		prev:          e.compactionState.PrevChecksum,
		firstChecksum: e.compactionTailChecksum,
	}
}

// next returns the checksum the event after rec chains from, given the one
// before it.
func (a *chainAnchor) next(rec EventRecord, last string) string {
	if a == nil {
		return computeEventChecksum(rec, last)
	}
	switch {
	case rec.Step < a.firstStep:
		return last // virtual prefix: not a stored row
	case rec.Step == a.firstStep && a.prev != "":
		return computeEventChecksum(rec, a.prev)
	case rec.Step == a.firstStep && a.firstChecksum != "":
		return a.firstChecksum
	}
	return computeEventChecksum(rec, last)
}
