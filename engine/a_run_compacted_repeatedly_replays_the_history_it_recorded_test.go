package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// cleat#3838: a second compaction used to build its state from the retained rows
// alone. The rebuilt history then held the events of the wrong steps, and a
// workflow that summed 30 signal payloads finished "done" with 1713 for a correct
// 1335, with nothing reported. The replay hands the guest whatever this rebuilt
// history says, so the property that matters is that it IS the history recorded.

// extractCompactionState is the first-compaction form of extractCompactionStateFrom.
// It lives here because production always has a previous state to pass (nil for
// the first pass), and a wrapper only tests call is dead code to the test-only
// guard.
func extractCompactionState(events []EventRecord) (*CompactionState, error) {
	return extractCompactionStateFrom(nil, events)
}

// sameRecordedEvent compares the fields chainEventMix writes, which are the ones
// a replay reads back for those event types.
func sameRecordedEvent(got, want EventRecord) error {
	if got.EventType != want.EventType || got.TimestampMs != want.TimestampMs {
		return fmt.Errorf("type/timestamp %s/%d, want %s/%d", got.EventType, got.TimestampMs, want.EventType, want.TimestampMs)
	}
	for _, f := range []struct{ name, got, want string }{
		{"service", got.Service, want.Service}, {"op", got.Op, want.Op},
		{"request", got.Request, want.Request}, {"response", got.Response, want.Response},
		{"signal_names", got.SignalNames, want.SignalNames}, {"signal_name", got.SignalName, want.SignalName},
		{"signal_payload", got.SignalPayload, want.SignalPayload},
		{"plugin_name", got.PluginName, want.PluginName}, {"plugin_func", got.PluginFunc, want.PluginFunc},
		{"plugin_input", got.PluginInput, want.PluginInput}, {"plugin_output", got.PluginOutput, want.PluginOutput},
	} {
		if f.got != f.want {
			return fmt.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if got.TimeoutMs != want.TimeoutMs {
		return fmt.Errorf("timeout_ms = %d, want %d", got.TimeoutMs, want.TimeoutMs)
	}
	return nil
}

func TestARunCompactedRepeatedlyReplaysTheHistoryItRecordedOnEveryStore(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)
			reader := store.(EventChecksumReader)

			id := fmt.Sprintf("chain-thrice-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, id, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			const threshold = 20
			recorded := chainEventMix(400)
			next := 0
			appendTo := func(n int) {
				t.Helper()
				for ; next < n; next++ {
					if err := store.AppendEventHistory(ctx, id, recorded[next]); err != nil {
						t.Fatalf("AppendEventHistory step %d: %v", next, err)
					}
				}
			}

			prevBoundary := 0
			for round := 1; round <= 3; round++ {
				appendTo(next + 40)

				// CONTROL: the worker would pick this run. The candidate query once
				// compared a step with a row count, which left a compacted run
				// waiting for a history compaction_step + threshold rows long.
				if round > 1 {
					ids, err := store.GetCompactionCandidates(ctx, threshold, 50)
					if err != nil {
						t.Fatalf("GetCompactionCandidates: %v", err)
					}
					found := false
					for _, c := range ids {
						found = found || c == id
					}
					if !found {
						t.Fatalf("round %d: a run with %d retained rows over a threshold of %d is not a compaction candidate", round, next-prevBoundary, threshold)
					}
				}
				pre := map[int]string{}
				for step := prevBoundary; step < next; step++ {
					sum, err := reader.StoredEventChecksum(ctx, id, step)
					if err != nil {
						t.Fatal(err)
					}
					pre[step] = sum
				}

				if err := CompactWorkflowHistory(ctx, store, id, threshold, nil); err != nil {
					t.Fatalf("round %d: CompactWorkflowHistory: %v", round, err)
				}
				cs, err := store.LoadCompactionState(ctx, id)
				if err != nil || cs == nil {
					t.Fatalf("round %d: LoadCompactionState = %v, %v", round, cs, err)
				}
				first := cs.firstRetainedStep()
				if first <= prevBoundary {
					t.Fatalf("round %d: the boundary did not move: %d after %d", round, first, prevBoundary)
				}
				if cs.CompactedStep != first || len(cs.Events) != first {
					t.Errorf("round %d: state covers CompactedStep=%d with %d events; first retained step is %d: it must cover exactly the steps before it", round, cs.CompactedStep, len(cs.Events), first)
				}
				if want := pre[first-1]; cs.PrevChecksum != want {
					t.Errorf("round %d: PrevChecksum = %q, want the stored checksum of step %d (%q)", round, cs.PrevChecksum, first-1, want)
				}
				if err := store.VerifyWorkflowEvents(ctx, id); err != nil {
					t.Fatalf("round %d: the run does not verify: %v", round, err)
				}

				// THE PROPERTY: what a replay is handed is the history that was
				// recorded, step for step, however many times it was compacted.
				tail, err := store.LoadEventHistory(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				full := buildFullHistoryFromCompaction(tail, cs)
				if len(full) != next {
					t.Fatalf("round %d: the rebuilt history has %d events, %d were recorded", round, len(full), next)
				}
				for i := range full {
					if full[i].Step != i && i < first {
						t.Fatalf("round %d: virtual event %d carries step %d", round, i, full[i].Step)
					}
					if err := sameRecordedEvent(full[i], recorded[i]); err != nil {
						t.Fatalf("round %d: the rebuilt history differs from the recorded one at step %d: %v", round, i, err)
					}
				}

				// And a resumed session chains from the last stored row.
				e := NewEngine(nil, nil, WithCompactionState(cs))
				s := &execSession{chainAnchor: e.chainAnchor()}
				for i := range full {
					s.advanceReplayStep(ctx, &full[i])
				}
				last, err := reader.StoredEventChecksum(ctx, id, next-1)
				if err != nil {
					t.Fatal(err)
				}
				if s.lastChecksum != last {
					t.Fatalf("round %d: a resumed session chains %q, the last stored row is %q", round, s.lastChecksum, last)
				}
				prevBoundary = first
			}

			// A run whose rows and state disagree about where the history starts is
			// refused by CompactWorkflowHistory, and refusing does not change what the
			// candidate query sees: it stays a candidate every pass, ten of them fill
			// a batch of ten, and no other run is ever compacted (cleat-review's
			// measurement on cleat#3842). The old second compaction produced exactly
			// this shape, so a database upgraded from 0.5.0 arrives with some.
			t.Run("runs the compactor refuses do not crowd a fresh run out", func(t *testing.T) {
				const refused = 3
				var refusedIDs []string
				for i := 0; i < refused; i++ {
					rid := fmt.Sprintf("chain-refused-%d-%d", i, time.Now().UnixNano())
					if _, _, err := store.StartNewRun(ctx, rid, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
						t.Fatalf("StartNewRun: %v", err)
					}
					for _, ev := range recorded[:110] {
						if err := store.AppendEventHistory(ctx, rid, ev); err != nil {
							t.Fatal(err)
						}
					}
					if err := CompactWorkflowHistory(ctx, store, rid, threshold, nil); err != nil {
						t.Fatalf("compact: %v", err)
					}
					for _, ev := range recorded[110:170] {
						if err := store.AppendEventHistory(ctx, rid, ev); err != nil {
							t.Fatal(err)
						}
					}
					// Rows 0..(boundary+2) go missing: the state still says the history
					// starts at the boundary, the rows say it starts three steps later.
					cs, err := store.LoadCompactionState(ctx, rid)
					if err != nil || cs == nil {
						t.Fatalf("no state: %v, %v", cs, err)
					}
					editChainState(t, store, "trim", rid, cs.firstRetainedStep()+3)
					if err := CompactWorkflowHistory(ctx, store, rid, threshold, nil); err == nil {
						t.Fatal("control: a state and rows that disagree were compacted, so this is not the refused shape")
					}
					refusedIDs = append(refusedIDs, rid)
				}
				fresh := fmt.Sprintf("chain-fresh-%d", time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, fresh, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatal(err)
				}
				for _, ev := range recorded[:30] {
					if err := store.AppendEventHistory(ctx, fresh, ev); err != nil {
						t.Fatal(err)
					}
				}
				ids, err := store.GetCompactionCandidates(ctx, threshold, refused)
				if err != nil {
					t.Fatalf("GetCompactionCandidates: %v", err)
				}
				got := map[string]bool{}
				for _, id := range ids {
					got[id] = true
				}
				if !got[fresh] {
					t.Errorf("a never-compacted run over the threshold was not offered in a batch of %d: got %v", refused, ids)
				}
				for _, id := range refusedIDs {
					if got[id] {
						t.Errorf("run %s, whose rows and state disagree, is offered for compaction every pass", id)
					}
				}
				// One pass, as the worker runs it: every offered run is compacted and
				// nothing in it is an error.
				for _, id := range ids {
					if err := CompactWorkflowHistory(ctx, store, id, threshold, nil); err != nil {
						t.Errorf("pass: %s: %v", id, err)
					}
				}
				if cs, err := store.LoadCompactionState(ctx, fresh); err != nil || cs == nil {
					t.Errorf("the fresh run was not compacted by the pass: %v, %v", cs, err)
				}
			})

			// With --encrypt-sensitive-payloads on, the state is sealed before it is
			// written (cleat#3774). The boundary checksum is added to the state
			// before that, and a second compaction reads the sealed state back,
			// extends it and seals it again: none of that may lose the anchor or the
			// events already in it.
			if pg, ok := store.(*PostgresStore); ok {
				t.Run("a sealed state is extended and keeps its boundary", func(t *testing.T) {
					enc, err := NewPayloadEncryption(validKey(t))
					if err != nil {
						t.Fatalf("NewPayloadEncryption: %v", err)
					}
					sealed := NewPostgresStore(pg.db).WithEncryption(enc, true)
					sid := fmt.Sprintf("chain-sealed-%d", time.Now().UnixNano())
					if _, _, err := sealed.StartNewRun(ctx, sid, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
						t.Fatalf("StartNewRun: %v", err)
					}
					sealedNext := 0
					for round := 1; round <= 2; round++ {
						for ; sealedNext < round*40; sealedNext++ {
							if err := sealed.AppendEventHistory(ctx, sid, recorded[sealedNext]); err != nil {
								t.Fatalf("append step %d: %v", sealedNext, err)
							}
						}
						if err := CompactWorkflowHistory(ctx, sealed, sid, threshold, nil); err != nil {
							t.Fatalf("round %d: CompactWorkflowHistory: %v", round, err)
						}
						cs, err := sealed.LoadCompactionState(ctx, sid)
						if err != nil || cs == nil {
							t.Fatalf("round %d: LoadCompactionState = %v, %v", round, cs, err)
						}
						if cs.PrevChecksum == "" {
							t.Fatalf("round %d: the sealed state lost its boundary checksum", round)
						}
						if err := sealed.VerifyWorkflowEvents(ctx, sid); err != nil {
							t.Fatalf("round %d: the run does not verify with a sealed state: %v", round, err)
						}
						tail, err := sealed.LoadEventHistory(ctx, sid)
						if err != nil {
							t.Fatal(err)
						}
						full := buildFullHistoryFromCompaction(tail, cs)
						if len(full) != sealedNext {
							t.Fatalf("round %d: rebuilt %d events, %d recorded", round, len(full), sealedNext)
						}
						for i := range full {
							if err := sameRecordedEvent(full[i], recorded[i]); err != nil {
								t.Fatalf("round %d: sealed state: step %d differs from the recorded one: %v", round, i, err)
							}
						}
					}
				})
			}
		})
	}
}

// ---- The state, without a database -----------------------------------------

func TestExtractCompactionStateFromExtendsThePreviousState(t *testing.T) {
	all := chainEventMix(60)
	// A child opened before the first boundary and closed after it, a defer
	// registered before it, and a child that stays open.
	all[3] = EventRecord{Step: 3, EventType: EventTypeChildWorkflow, ChildName: "c1", ChildInput: `{"a":1}`, RunID: "run-1"}
	all[7] = EventRecord{Step: 7, EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup"}
	all[9] = EventRecord{Step: 9, EventType: EventTypeChildWorkflow, ChildName: "c2", ChildInput: `{"b":2}`, RunID: "run-2"}
	all[25] = EventRecord{Step: 25, EventType: EventTypeAwaitChild, RunID: "run-1", Response: `{"done":true}`}

	once, err := extractCompactionState(all[:20])
	if err != nil {
		t.Fatal(err)
	}
	twice, err := extractCompactionStateFrom(once, all[20:40])
	if err != nil {
		t.Fatal(err)
	}
	wantAll, err := extractCompactionState(all[:40])
	if err != nil {
		t.Fatal(err)
	}

	a, _ := json.Marshal(twice.Events)
	b, _ := json.Marshal(wantAll.Events)
	if string(a) != string(b) || twice.CompactedStep != wantAll.CompactedStep || twice.CompactedStep != 40 {
		t.Errorf("extending the state is not the same as compacting everything at once: CompactedStep %d/%d, %d/%d events",
			twice.CompactedStep, wantAll.CompactedStep, len(twice.Events), len(wantAll.Events))
	}
	if len(twice.PendingDefers) != 1 || twice.PendingDefers[0].ID != "d1" {
		t.Errorf("a defer registered before the first boundary was lost: %+v", twice.PendingDefers)
	}
	if len(twice.OpenChildren) != 1 || twice.OpenChildren[0].RunID != "run-2" || twice.OpenChildren[0].Name != "c2" {
		t.Errorf("open children after the second pass = %+v, want only run-2 (run-1 closed at step 25)", twice.OpenChildren)
	}
	if len(once.Events) != 20 {
		t.Errorf("extending mutated the previous state: %d events", len(once.Events))
	}
}

func TestExtractCompactionStateFromKeepsTheTruncationCount(t *testing.T) {
	first, _ := chainedEvents(0, DefaultMaxCompactedEvents+7, "")
	once, err := extractCompactionState(first)
	if err != nil {
		t.Fatal(err)
	}
	if got := truncatedCompactedEvents(once); got != 7 {
		t.Fatalf("control: first pass truncated %d, want 7", got)
	}
	more, _ := chainedEvents(DefaultMaxCompactedEvents+7, DefaultMaxCompactedEvents+7+30, "")
	twice, err := extractCompactionStateFrom(once, more)
	if err != nil {
		t.Fatal(err)
	}
	if got := truncatedCompactedEvents(twice); got != 37 {
		t.Errorf("truncated after the second pass = %d, want 7 + 30", got)
	}
	if twice.firstRetainedStep() != DefaultMaxCompactedEvents+7+30 {
		t.Errorf("first retained step = %d, want %d", twice.firstRetainedStep(), DefaultMaxCompactedEvents+7+30)
	}
	if len(twice.Events) != DefaultMaxCompactedEvents {
		t.Errorf("%d events kept, want the cap %d", len(twice.Events), DefaultMaxCompactedEvents)
	}

	// No new truncation, previous truncation: the count must carry over.
	small, _ := chainedEvents(DefaultMaxCompactedEvents+7, DefaultMaxCompactedEvents+7, "")
	same, err := extractCompactionStateFrom(once, small)
	if err != nil || truncatedCompactedEvents(same) != 7 {
		t.Errorf("an empty extension dropped the truncation count: %v, %d", err, truncatedCompactedEvents(same))
	}
}

func TestCompactWorkflowHistoryKeepsByStepAfterAnEarlierCompaction(t *testing.T) {
	// The rows left by a first compaction start at step 100, not 0.
	events := make([]EventRecord, 60)
	for i := range events {
		events[i] = EventRecord{Step: 100 + i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", 100+i)}
	}
	prev, err := extractCompactionState(func() []EventRecord {
		var e []EventRecord
		for i := 0; i < 100; i++ {
			e = append(e, EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)})
		}
		return e
	}())
	if err != nil {
		t.Fatal(err)
	}
	st := &mockCompactStore{events: events, loadCompactionStateResult: prev}
	if err := CompactWorkflowHistory(context.Background(), st, "wf", 40, nil); err != nil {
		t.Fatal(err)
	}
	if st.compactCount != 1 {
		t.Fatalf("CompactHistory ran %d times, want 1", st.compactCount)
	}
	// 60 rows, threshold 40: keep the last 20, so the first kept row is step 140.
	if st.keepStep != 140 || st.compactStep != 140 {
		t.Errorf("keepStep/compactStep = %d/%d, want 140/140: a position in the rows is not a step", st.keepStep, st.compactStep)
	}
	var cs CompactionState
	if err := json.Unmarshal(st.compactState, &cs); err != nil {
		t.Fatal(err)
	}
	if cs.CompactedStep != 140 || len(cs.Events) != 140 {
		t.Errorf("state covers %d steps with %d events, want 140 and 140", cs.CompactedStep, len(cs.Events))
	}

	// A history that does not start where its state says is not extended.
	wrong := &mockCompactStore{events: events, loadCompactionStateResult: &CompactionState{Version: 1, CompactedStep: 90}}
	if err := CompactWorkflowHistory(context.Background(), wrong, "wf", 40, nil); err == nil || wrong.compactCount != 0 {
		t.Errorf("a state and history that disagree were compacted: err=%v, ran=%d", err, wrong.compactCount)
	}
}
