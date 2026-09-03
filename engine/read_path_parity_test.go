package engine

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// Every dialect has three ways to read event history -- LoadEventHistory,
// LoadEventHistoryPaginated and StreamEventHistory -- and nothing held them to
// the same answer. Nine implementations, each with its own SELECT and its own
// scan, and the differences were invisible because every existing test picks
// one path and checks it against itself.
//
// Measured 2026-09-03, before this change:
//
//	dialect     path         TimestampMs                     CreatedAt
//	postgres    Load         truncated to whole seconds      ok
//	postgres    Paginated    never set (0)                   ok
//	postgres    Stream       truncated to whole seconds      ok
//	mysql       Load         ok                              zero time
//	mysql       Paginated    never set (0)                   zero time
//	mysql       Stream       ok                              zero time
//	sqlserver   Load         ok                              zero time
//	sqlserver   Paginated    never set (0)                   zero time
//	sqlserver   Stream       ok                              zero time
//
// The zeroes matter more than they look. TimestampMs is the replay virtual
// clock: execSession.Now (lifecycle.go) returns the previous history event's
// TimestampMs, so an event that reads back as 0 makes Now() fall through to
// wall-clock time, and one that reads back truncated makes Now() return a
// value the fresh run never produced. On PostgreSQL -- the default dialect --
// a workflow resuming after a crash saw a Now() up to 999ms earlier than the
// run that recorded it.

// Three paths, and only three: these are the ones on the WorkflowStore
// interface (store_interface.go), which is what every caller outside this
// package can reach.
//
// There is a FOURTH EventRecord reader in the package, DBEventStream
// (engine/event_stream.go), and it is deliberately not covered here. Measured
// 2026-09-03: `grep -rn NewDBEventStream --include=*.go .` finds only its own
// tests, so nothing constructs one in production, and its query is worse than
// anything this file fixes -- it selects no `payload` column at all, so every
// payload-carried field comes back empty, and its WHERE clause is
// `workflow_id = $1` with **no tenant_id predicate**, so if it were ever wired
// up it would read across tenants. Testing it here would mean asserting
// against code with no callers; deleting it or fixing it is a decision for
// whoever owns that type. IMPROVEMENT-PLAN records it.

// readAllPaths returns the history a store gives back through each of its
// three read paths, keyed by path name.
func readAllPaths(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) map[string][]EventRecord {
	t.Helper()
	out := map[string][]EventRecord{}

	load, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	out["LoadEventHistory"] = load

	page, err := store.LoadEventHistoryPaginated(ctx, wfID, 0, 100)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	out["LoadEventHistoryPaginated"] = page

	ch, errCh := store.StreamEventHistory(ctx, wfID, 100)
	var streamed []EventRecord
	for ev := range ch {
		streamed = append(streamed, ev)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("StreamEventHistory: %v", err)
	}
	out["StreamEventHistory"] = streamed

	return out
}

// diffRecords returns the names of the exported EventRecord fields on which a
// and b disagree.
func diffRecords(a, b EventRecord) []string {
	var diffs []string
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	rt := av.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Name == "Pending" {
			// Derived at load time from intent_at/checksum, and only
			// LoadEventHistory's SELECT computes it. A genuine difference in
			// what the paths are for, not a difference about the row.
			continue
		}
		if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			diffs = append(diffs, f.Name)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// The event this file writes and expects back, unchanged, from every path.
// TimestampMs deliberately carries milliseconds that are not a whole second:
// a value like 1756900000000 round-trips through a truncating read and proves
// nothing.
func parityProbeEvent() EventRecord {
	return EventRecord{
		Step:         0,
		EventType:    EventTypePluginCall,
		TimestampMs:  1756900000123,
		PluginName:   "test-plugin",
		PluginFunc:   "Echo",
		PluginInput:  `{"in":1}`,
		PluginOutput: `{"out":2}`,
	}
}

func TestEveryReadPathReturnsTheSameRow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "read-path-parity")
		if err := store.AppendEventHistory(ctx, wfID, parityProbeEvent()); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}

		paths := readAllPaths(t, ctx, store, wfID)
		ref := paths["LoadEventHistory"]
		if len(ref) != 1 {
			t.Fatalf("LoadEventHistory returned %d events, want 1", len(ref))
		}
		for _, name := range []string{"LoadEventHistoryPaginated", "StreamEventHistory"} {
			got := paths[name]
			if len(got) != 1 {
				t.Errorf("%s returned %d events, want 1", name, len(got))
				continue
			}
			if diffs := diffRecords(ref[0], got[0]); len(diffs) > 0 {
				t.Errorf("%s disagrees with LoadEventHistory about the same row on %v -- "+
					"whichever of the two is wrong, a caller's answer depends on which "+
					"method it happened to call", name, diffs)
			}
		}
	})
}

// Agreement is not correctness: three paths that all return zero agree
// perfectly. This is the half that says what the answer has to be.
func TestEveryReadPathReturnsTheTimeThatWasWritten(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "read-path-time")
		want := parityProbeEvent()
		if err := store.AppendEventHistory(ctx, wfID, want); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}

		for name, got := range readAllPaths(t, ctx, store, wfID) {
			if len(got) != 1 {
				t.Errorf("%s returned %d events, want 1", name, len(got))
				continue
			}
			if got[0].TimestampMs != want.TimestampMs {
				t.Errorf("%s: TimestampMs = %d, want %d (off by %d ms) -- this is the replay "+
					"virtual clock, so a workflow resuming after a crash sees a Now() the run "+
					"that recorded it never returned",
					name, got[0].TimestampMs, want.TimestampMs,
					want.TimestampMs-got[0].TimestampMs)
			}
			if got[0].CreatedAt.IsZero() {
				t.Errorf("%s: CreatedAt is the zero time, so an admin timeline has no time to "+
					"show for this event", name)
			}
			if ms := got[0].CreatedAt.UnixMilli(); ms != want.TimestampMs {
				t.Errorf("%s: CreatedAt is %v (%d ms) but TimestampMs is %d; the two are read "+
					"from one column and must not disagree",
					name, got[0].CreatedAt, ms, got[0].TimestampMs)
			}
		}
	})
}
