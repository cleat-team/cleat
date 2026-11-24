package engine

import (
	"testing"
	"time"
)

// cleat#4277. The marker that tells an await's own answer from the next call's first-read settlement is a field of the record, and a field added to a
// record is dropped by every encoder that was not told about it, which nothing notices because the record still reads as a settlement (the lesson of
// cleat#4325). Three encodings carry it, and each is pinned here against the others: the checksum payload and its loader (what every store reads
// through), and compaction in BOTH directions.

// The checksums an Err-free and FirstRead-free await, resolved and rejected had on develop 6ae87e8c, computed there by a scratch test in a worktree at
// that commit and not by the code under test. A test that compared computeEventChecksum with itself would pass if the payload ALWAYS carried a
// "first_read" key, which changes the checksum of every promise event ever recorded and breaks verification of every old history.
func TestAPromiseEventWrittenBeforeTheFirstReadMarkerKeepsItsChecksum(t *testing.T) {
	const prev = "00112233445566ff"
	cases := []struct {
		name           string
		rec            EventRecord
		first, chained string
	}{
		{"await", EventRecord{Step: 2, EventType: EventTypeAwaitPromise, PromiseID: "p-1", TimeoutMs: 100, TimestampMs: 1000}, "bb30cc5f9c0de448", "8b181782b5584989"},
		{"resolved", EventRecord{Step: 3, EventType: EventTypePromiseResolved, PromiseID: "p-1", PromiseResult: `"done"`, TimestampMs: 1001}, "d3abbe4c777393de", "28136bbfc40625d3"},
		{"rejected", EventRecord{Step: 4, EventType: EventTypePromiseRejected, PromiseID: "p-1", PromiseError: "declined", TimestampMs: 1002}, "996d32f90690e407", "64976ec211683501"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeEventChecksum(c.rec, ""); got != c.first {
				t.Errorf("first-in-chain checksum = %s, want %s (as computed before FirstRead existed)", got, c.first)
			}
			if got := computeEventChecksum(c.rec, prev); got != c.chained {
				t.Errorf("chained checksum = %s, want %s", got, c.chained)
			}
			if c.rec.EventType == EventTypeAwaitPromise {
				return // the marker is never set on an await
			}
			marked := c.rec
			marked.FirstRead = true
			if computeEventChecksum(marked, "") == c.first {
				t.Errorf("a marked %s hashes the same as an unmarked one: an edited or removed marker would not break the chain, and it changes what a replay answers", c.name)
			}
		})
	}
}

func TestAFirstReadMarkerSurvivesThePayloadRoundTrip(t *testing.T) {
	for _, typ := range []EventType{EventTypePromiseResolved, EventTypePromiseRejected} {
		t.Run(string(typ), func(t *testing.T) {
			rec := EventRecord{Step: 3, EventType: typ, PromiseID: "p-1", PromiseResult: "v", PromiseError: "boom", FirstRead: true}
			payload, err := eventRecordToPayload(rec)
			if err != nil {
				t.Fatalf("eventRecordToPayload: %v", err)
			}
			var back EventRecord
			populateFromPayload(&back, payload)
			if !back.FirstRead {
				t.Errorf("the marker did not survive the payload: %s", payload)
			}
			// The control: an unmarked record does not come back marked, and carries no key at all.
			rec.FirstRead = false
			payload, _ = eventRecordToPayload(rec)
			back = EventRecord{}
			populateFromPayload(&back, payload)
			if back.FirstRead {
				t.Errorf("an unmarked record came back marked: %s", payload)
			}
		})
	}
}

// Through the real compaction functions, and then through a REPLAY of what they expand to: the structural check shows the field came back, the replay
// shows the answer it exists for. Both outcomes.
func TestAFirstReadMarkerSurvivesCompaction(t *testing.T) {
	longAgo := time.Now().UnixMilli() - 3_600_000
	for _, c := range []struct {
		name    string
		settled EventRecord
	}{
		{"resolved", EventRecord{Step: 1, EventType: EventTypePromiseResolved, PromiseID: "p1", PromiseResult: "v", FirstRead: true}},
		{"rejected", EventRecord{Step: 1, EventType: EventTypePromiseRejected, PromiseID: "p1", PromiseError: "boom", FirstRead: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			events := []EventRecord{
				{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 100, TimestampMs: longAgo},
				c.settled,
			}
			cs, err := extractCompactionState(events)
			if err != nil {
				t.Fatalf("extractCompactionState: %v", err)
			}
			full := buildFullHistoryFromCompaction(nil, cs)
			if len(full) != len(events) {
				t.Fatalf("expanded history has %d events, recorded %d", len(full), len(events))
			}
			if !full[1].FirstRead {
				t.Fatalf("compaction dropped the marker: %+v", full[1])
			}
			// The control: the await is not marked, and a settlement recorded WITHOUT the marker expands unmarked.
			if full[0].FirstRead {
				t.Errorf("compaction marked the await: %+v", full[0])
			}
			plain := events[1]
			plain.FirstRead = false
			cs2, _ := extractCompactionState([]EventRecord{events[0], plain})
			if buildFullHistoryFromCompaction(nil, cs2)[1].FirstRead {
				t.Errorf("compaction marked a settlement that was not")
			}

			// The answer the marker exists for: a compacted history replays the first await as the timeout it was.
			runPromiseRow(t, promiseRow{
				name: "replay of the expanded history", status: "resolved", result: "v", hist: full, timeoutMs: 100,
				want: promiseWant{timedOut: true, step: 1, replaying: true},
			})
		})
	}
}

// The spec validation rule: the hook only makes sense for a run that can end without a final record.
func TestAStepSpecWithOpensACallNeedsARunThatCanEndWithoutAFinal(t *testing.T) {
	sp := awaitPromiseStep
	if err := sp.validate(); err != nil {
		t.Fatalf("PRECONDITION FAILED: the await's own spec does not validate: %v", err)
	}
	sp.EndsWithoutFinal = false
	if err := sp.validate(); err == nil {
		t.Errorf("a spec with OpensACall and no way to end a run without a final record validated")
	}
}
