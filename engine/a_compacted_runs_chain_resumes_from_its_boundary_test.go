package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cleat#3837: compaction deletes the leading event rows, so the first row it
// keeps chains its checksum from a row that no longer exists. Verification and
// replay both started that row from an empty predecessor, and every compacted
// run that resumed failed with "checksum mismatch" at its first retained step.

// chainedEvents builds events [from, to) with a checksum chain starting at prev and
// returns them with their stored checksums keyed by step.
func chainedEvents(from, to int, prev string) ([]EventRecord, map[int]string) {
	var events []EventRecord
	stored := map[int]string{}
	for step := from; step < to; step++ {
		ev := EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", step)}
		prev = computeEventChecksum(ev, prev)
		stored[step] = prev
		events = append(events, ev)
	}
	return events, stored
}

func TestVerifyEventChainStartsFromTheCompactionBoundary(t *testing.T) {
	const boundary = 10
	// Rows 0-19 were written as one chain; 0-9 were then compacted away.
	all, allSums := chainedEvents(0, 20, "")
	tail, tailSums := all[boundary:], map[int]string{}
	for s := boundary; s < 20; s++ {
		tailSums[s] = allSums[s]
	}
	cs := &CompactionState{Version: 1, CompactedStep: boundary, PrevChecksum: allSums[boundary-1]}

	// THE BUG: with no state the tail's first row chains from nothing, and an
	// intact history is reported corrupt. This is what every resume did.
	if err := verifyEventChain("wf", tail, tailSums, nil); err == nil {
		t.Fatal("control: an intact compacted tail verified WITHOUT its boundary; the test cannot show the bug")
	} else if !strings.Contains(err.Error(), fmt.Sprintf("step %d: checksum mismatch", boundary)) {
		t.Fatalf("control failed for the wrong reason: %v", err)
	}

	t.Run("an intact tail verifies from the stored boundary", func(t *testing.T) {
		if err := verifyEventChain("wf", tail, tailSums, cs); err != nil {
			t.Fatalf("intact compacted history reported corrupt: %v", err)
		}
	})

	t.Run("the first retained row is verified, not trusted", func(t *testing.T) {
		bad := append([]EventRecord(nil), tail...)
		bad[0].Op = "tampered"
		err := verifyEventChain("wf", bad, tailSums, cs)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("step %d: checksum mismatch", boundary)) {
			t.Fatalf("a changed first retained row went undetected: %v", err)
		}
	})

	t.Run("a changed later row is caught", func(t *testing.T) {
		bad := append([]EventRecord(nil), tail...)
		bad[4].Op = "tampered"
		if err := verifyEventChain("wf", bad, tailSums, cs); err == nil {
			t.Fatal("a changed later row went undetected")
		}
	})

	t.Run("a wrong boundary is caught", func(t *testing.T) {
		wrong := *cs
		wrong.PrevChecksum = allSums[boundary-2]
		if err := verifyEventChain("wf", tail, tailSums, &wrong); err == nil {
			t.Fatal("a boundary that is not the first row's predecessor verified")
		}
	})

	t.Run("a gap at the start fails closed", func(t *testing.T) {
		err := verifyEventChain("wf", tail[2:], tailSums, cs)
		if err == nil || !strings.Contains(err.Error(), "first retained step") {
			t.Fatalf("history starting two rows late verified: %v", err)
		}
	})

	t.Run("a truncated state counts the truncated events", func(t *testing.T) {
		trunc := *cs
		trunc.CompactedStep = 4
		trunc.Summary = &TruncationSummary{TruncatedCount: boundary - 4}
		if err := verifyEventChain("wf", tail, tailSums, &trunc); err != nil {
			t.Fatalf("a truncated state moved the first retained step: %v", err)
		}
	})
}

// A state written before the fix has no boundary. It must still let a run that
// was compacted and not yet resumed recover, and what it gives up is exactly the
// first retained row.
func TestVerifyEventChainLegacyStateTrustsOnlyTheFirstRetainedRow(t *testing.T) {
	const boundary = 10
	all, allSums := chainedEvents(0, 20, "")
	tail, tailSums := all[boundary:], map[int]string{}
	for s := boundary; s < 20; s++ {
		tailSums[s] = allSums[s]
	}
	legacy := &CompactionState{Version: 1, CompactedStep: boundary}

	if err := verifyEventChain("wf-legacy", tail, tailSums, legacy); err != nil {
		t.Fatalf("a run compacted before the fix cannot recover: %v", err)
	}

	// Known-positive, stated as expected: the one row the fallback trusts.
	first := append([]EventRecord(nil), tail...)
	first[0].Op = "tampered"
	if err := verifyEventChain("wf-legacy", first, tailSums, legacy); err != nil {
		t.Fatalf("the legacy fallback is documented to trust the first row, but it rejected it: %v", err)
	}

	// And nothing after it.
	later := append([]EventRecord(nil), tail...)
	later[1].Op = "tampered"
	if err := verifyEventChain("wf-legacy", later, tailSums, legacy); err == nil {
		t.Fatal("the legacy fallback trusted a row after the first")
	}
}

func TestWithBoundaryChecksumKeepsEveryOtherField(t *testing.T) {
	in, _ := json.Marshal(&CompactionState{Version: 1, CompactedStep: 3, QueryState: map[string]string{"a": "b"}})
	// A field a newer writer added must survive a round trip through the editor.
	in = []byte(strings.Replace(string(in), `{`, `{"from_the_future":7,`, 1))
	out, err := withBoundaryChecksum(in, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	var cs CompactionState
	if err := json.Unmarshal(out, &cs); err != nil {
		t.Fatal(err)
	}
	if cs.PrevChecksum != "abc123" || cs.CompactedStep != 3 || cs.QueryState["a"] != "b" {
		t.Errorf("state after edit = %+v", cs)
	}
	if !strings.Contains(string(out), `"from_the_future":7`) {
		t.Errorf("an unknown field was dropped: %s", out)
	}
	if same, _ := withBoundaryChecksum(in, ""); string(same) != string(in) {
		t.Errorf("an empty checksum changed the state: %s", same)
	}
}

// Replay must leave lastChecksum equal to the stored checksum of the last row it
// consumed, or the first event written after a resume chains from the wrong
// predecessor and fails verification on the NEXT resume.
//
// Chaining over the virtual prefix happens to reproduce the stored chain when
// every compacted event is rebuilt losslessly and nothing was truncated. It does
// not once the state is truncated (DefaultMaxCompactedEvents), because the
// oldest events are gone and the chain is then folded over a different set.
func TestReplayOverACompactedHistorySeedsTheChainFromTheBoundary(t *testing.T) {
	for _, compacted := range []int{10, DefaultMaxCompactedEvents + 5} {
		boundary := compacted
		all, allSums := chainedEvents(0, boundary+10, "")
		tail := all[boundary:]
		last := allSums[boundary+9]

		prefix, err := extractCompactionState(all[:boundary])
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			cs   func() *CompactionState
			tail string
		}{
			{"with a boundary", func() *CompactionState { c := *prefix; c.PrevChecksum = allSums[boundary-1]; return &c }, ""},
			{"legacy, tail checksum supplied", func() *CompactionState { c := *prefix; return &c }, allSums[boundary]},
		} {
			t.Run(fmt.Sprintf("%d compacted, %s", compacted, c.name), func(t *testing.T) {
				cs := c.cs()
				e := NewEngine(nil, nil, WithCompactionState(cs), WithCompactionTailChecksum(c.tail))
				full := buildFullHistoryFromCompaction(tail, cs)
				s := &execSession{chainAnchor: e.chainAnchor()}
				for i := range full {
					s.advanceReplayStep(context.Background(), &full[i])
				}
				if s.lastChecksum != last {
					t.Errorf("lastChecksum after replay = %q, want the last stored checksum %q", s.lastChecksum, last)
				}
			})
		}

		if compacted > DefaultMaxCompactedEvents {
			t.Run("without an anchor a truncated state chains wrongly, which was the bug", func(t *testing.T) {
				cs := *prefix
				cs.PrevChecksum = allSums[boundary-1]
				full := buildFullHistoryFromCompaction(tail, &cs)
				s := &execSession{} // what every session was before cleat#3837
				for i := range full {
					s.advanceReplayStep(context.Background(), &full[i])
				}
				if s.lastChecksum == last {
					t.Fatal("control: a session with no anchor already gets the right chain; the test cannot show the bug")
				}
			})
		}
	}
}

// ---- The same, against each real store -------------------------------------

// chainEventMix returns n events of the kinds a long-running workflow really
// writes, so the compaction round trip is not only over one lossless type.
func chainEventMix(n int) []EventRecord {
	out := make([]EventRecord, n)
	for i := range out {
		ev := EventRecord{Step: i, TimestampMs: int64(1_700_000_000_000 + i*1000)}
		switch i % 4 {
		case 0:
			ev.EventType, ev.Service, ev.Op, ev.Request, ev.Response = EventTypeCall, "svc", fmt.Sprintf("op%d", i), `{"n":`+fmt.Sprint(i)+`}`, `{"ok":true}`
		case 1:
			ev.EventType, ev.SignalNames, ev.TimeoutMs = EventTypeAwaitSignals, `["__evt:x"]`, 6000
		case 2:
			ev.EventType, ev.SignalName, ev.SignalPayload = EventTypeSignalReceived, "__evt:x", `{"i":`+fmt.Sprint(i)+`}`
		case 3:
			ev.EventType, ev.PluginName, ev.PluginFunc, ev.PluginInput, ev.PluginOutput = EventTypePluginCall, "p", "f", `{"i":`+fmt.Sprint(i)+`}`, `{"found":false}`
		}
		out[i] = ev
	}
	return out
}

// editChainState runs one statement against the store's own tables, outside the
// engine. It is the only way to damage a row or age a state the way a real
// history gets damaged or aged, and it is per-dialect for that reason.
func editChainState(t *testing.T, store WorkflowStore, what string, args ...any) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch s := store.(type) {
	case *PostgresStore:
		q := map[string]string{
			"checksum": `UPDATE event_history SET checksum = 'deadbeefdeadbeef' WHERE workflow_id = $1 AND step = $2`,
			"legacy":   `UPDATE workflow_payloads SET compaction_state = compaction_state - 'prev_checksum' WHERE id = $1`,
		}[what]
		_, err = s.db.ExecContext(ctx, q, args...)
	case *MySQLStore:
		q := map[string]string{
			"checksum": `UPDATE event_history SET checksum = 'deadbeefdeadbeef' WHERE workflow_id = ? AND step = ?`,
			"legacy":   `UPDATE workflow_instances SET compaction_state = JSON_REMOVE(compaction_state, '$.prev_checksum') WHERE id = ?`,
		}[what]
		_, err = s.db.ExecContext(ctx, q, args...)
	case *MSSQLStore:
		q := map[string]string{
			"checksum": `UPDATE event_history SET checksum = 'deadbeefdeadbeef' WHERE workflow_id = @p1 AND step = @p2`,
			"legacy":   `UPDATE workflow_instances SET compaction_state = JSON_MODIFY(compaction_state, '$.prev_checksum', NULL) WHERE id = @p1`,
		}[what]
		var tx *sql.Tx
		if tx, err = s.beginTxWithContext(ctx); err == nil {
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, q, args...); err == nil {
				err = tx.Commit()
			}
		}
	default:
		t.Fatalf("editChainState: no statements for %T", store)
	}
	if err != nil {
		t.Fatalf("editChainState(%s): %v", what, err)
	}
}

func TestACompactedRunVerifiesAndResumesOnEveryStore(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			reader, ok := store.(EventChecksumReader)
			if !ok {
				t.Fatalf("%T does not implement EventChecksumReader", store)
			}
			stored := func(id string, step int) string {
				t.Helper()
				sum, err := reader.StoredEventChecksum(ctx, id, step)
				if err != nil {
					t.Fatalf("StoredEventChecksum(%d): %v", step, err)
				}
				return sum
			}

			newRun := func(name string) string {
				t.Helper()
				id := fmt.Sprintf("chain-%s-%d", name, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				return id
			}
			appendEvents := func(id string, from, to int) {
				t.Helper()
				for _, ev := range chainEventMix(to)[from:to] {
					if err := store.AppendEventHistory(ctx, id, ev); err != nil {
						t.Fatalf("AppendEventHistory step %d: %v", ev.Step, err)
					}
				}
			}
			// compact is the worker's own entry point, with the threshold the
			// worker would use: 40 events over a threshold of 20 keeps the last 10.
			compact := func(id string, threshold int) {
				t.Helper()
				if err := CompactWorkflowHistory(ctx, store, id, threshold, nil); err != nil {
					t.Fatalf("CompactWorkflowHistory: %v", err)
				}
			}

			t.Run("verifies after compaction, and a resumed session chains from the stored tail", func(t *testing.T) {
				id := newRun("resume")
				appendEvents(id, 0, 40)

				// CONTROL: the history verifies before it is compacted. Without it
				// a verify that always failed would look like the bug.
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("control: an intact, uncompacted history failed verification: %v", err)
				}
				wantBoundary := stored(id, 29)

				compact(id, 20)
				cs, err := store.LoadCompactionState(ctx, id)
				if err != nil || cs == nil {
					t.Fatalf("LoadCompactionState = %v, %v; compaction did not run", cs, err)
				}
				if cs.CompactedStep != 30 {
					t.Fatalf("CompactedStep = %d, want 30 (40 events, threshold 20 keeps 10)", cs.CompactedStep)
				}
				if cs.PrevChecksum == "" || cs.PrevChecksum != wantBoundary {
					t.Fatalf("PrevChecksum = %q, want the stored checksum of the last deleted row, %q", cs.PrevChecksum, wantBoundary)
				}
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("a compacted run failed verification on resume: %v", err)
				}

				// A resumed session: replay the virtual prefix and the tail through
				// the replayer, then write one more event the way the engine does.
				tail, err := store.LoadEventHistory(ctx, id)
				if err != nil {
					t.Fatalf("LoadEventHistory: %v", err)
				}
				e := NewEngine(nil, nil, WithCompactionState(cs))
				full := buildFullHistoryFromCompaction(tail, cs)
				s := &execSession{chainAnchor: e.chainAnchor()}
				for i := range full {
					s.advanceReplayStep(ctx, &full[i])
				}
				if got, want := s.lastChecksum, stored(id, 39); got != want {
					t.Fatalf("lastChecksum after replay = %q, want the stored checksum of the last row %q", got, want)
				}
				next := chainEventMix(41)[40]
				inMemory := computeEventChecksum(next, s.lastChecksum)
				if err := store.AppendEventHistory(ctx, id, next); err != nil {
					t.Fatalf("append after resume: %v", err)
				}
				if got := stored(id, 40); got != inMemory {
					t.Fatalf("the event written after a resume stored checksum %q; the session chained %q", got, inMemory)
				}
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("the run does not verify after the resumed append: %v", err)
				}
			})

			t.Run("a corrupted first retained row is caught", func(t *testing.T) {
				id := newRun("corrupt-first")
				appendEvents(id, 0, 40)
				compact(id, 20)
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("control: the compacted run does not verify before it is damaged: %v", err)
				}
				editChainState(t, store, "checksum", id, 30)
				err := store.VerifyWorkflowEvents(ctx, id)
				if err == nil || !strings.Contains(err.Error(), "step 30: checksum mismatch") {
					t.Fatalf("damage to the first retained row went undetected on the new path: %v", err)
				}
			})

			t.Run("a state compacted before the fix recovers, and its later rows are still checked", func(t *testing.T) {
				id := newRun("legacy")
				appendEvents(id, 0, 40)
				compact(id, 20)
				editChainState(t, store, "legacy", id)
				cs, err := store.LoadCompactionState(ctx, id)
				if err != nil || cs == nil || cs.PrevChecksum != "" {
					t.Fatalf("control: the state still carries a boundary after stripping it: %+v, %v", cs, err)
				}
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("a legacy compacted run cannot recover: %v", err)
				}
				// What the fallback gives up -- a change to the first retained row's
				// CONTENT -- is pinned by the DB-free test above; damaging the stored
				// checksum here would instead move the anchor and fail row 31.
				// And rows after it are still checked.
				editChainState(t, store, "checksum", id, 33)
				if err := store.VerifyWorkflowEvents(ctx, id); err == nil {
					t.Fatal("the legacy fallback let a damaged later row through")
				}
			})

		})
	}
}
