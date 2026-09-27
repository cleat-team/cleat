package engine

import (
	"strings"
	"testing"
)

// The channel cleat#902 needed: whether a call ran out of retries has to reach
// the worker, and it cannot go through the guest -- the error crosses that
// boundary as a plain string. These tests cover the three places it could be
// dropped between engine/durablecalls.go setting it and the worker reading it.

func TestRetriesExhaustedRoundTripsThroughPayload(t *testing.T) {
	for _, want := range []bool{true, false} {
		rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op",
			Request: `{}`, Err: "retries exhausted: boom", RetriesExhausted: want}

		payload, err := eventRecordToPayload(rec)
		if err != nil {
			t.Fatalf("eventRecordToPayload: %v", err)
		}

		got := EventRecord{Step: 0, EventType: EventTypeCall}
		populateFromPayload(&got, payload)
		if got.RetriesExhausted != want {
			t.Errorf("RetriesExhausted=%v did not survive the payload round trip (payload: %s)", want, payload)
		}
	}
}

// TestCompactionPreservesRetriesExhausted covers the gap compaction.go's own
// comment warns about for the fields beside this one: a compacted region that
// dropped the bit would reconstruct an exhaustion as an ordinary failure, so
// the oldest history -- the history most likely to have been compacted -- would
// silently stop dead-lettering.
func TestCompactionPreservesRetriesExhausted(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op",
			Request: `{}`, Err: "retries exhausted: connection refused", RetriesExhausted: true},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2",
			Request: `{}`, Err: "connection reset", RetriesExhausted: false},
	}

	cs, err := extractCompactionState(events)
	if err != nil {
		t.Fatalf("extractCompactionState: %v", err)
	}
	got := buildFullHistoryFromCompaction(nil, cs)
	if len(got) != len(events) {
		t.Fatalf("expected %d reconstructed events, got %d", len(events), len(got))
	}
	if !got[0].RetriesExhausted {
		t.Errorf("event 0: RetriesExhausted lost in compaction round-trip: got false, want true "+
			"(original: %+v)", events[0])
	}
	if got[1].RetriesExhausted {
		t.Error("event 1: an ordinary failure reconstructed as an exhaustion")
	}
}

// TestLegacyFailureIsNotAnExhaustion pins the compatibility default.
//
// Every call failure in every event_history in existence was written without
// this key, and none of them was classified. They must read back false, or the
// upgrade would retroactively dead-letter workflows that failed for reasons
// nobody ever called a retry exhaustion.
func TestLegacyFailureIsNotAnExhaustion(t *testing.T) {
	legacy := []byte(`{"service":"svc","operation":"op","error":"retries exhausted: boom"}`)
	rec := EventRecord{Step: 0, EventType: EventTypeCall}
	populateFromPayload(&rec, legacy)

	if rec.RetriesExhausted {
		t.Fatal("a payload with no retries_exhausted key read back as an exhaustion; " +
			"every pre-existing call failure would be reclassified by the upgrade")
	}
}

// TestAnOrdinaryFailuresChecksumDoesNotMove is the half that protects history
// already written.
//
// The checksum is taken over eventRecordToPayload's bytes, and every event ever
// recorded chains off the one before it. If adding this field changed the
// payload of an event that is NOT an exhaustion -- which is almost all of them
// -- every subsequent checksum in every workflow would fail to reverify, and
// integrity checking would report corruption across the estate.
//
// Writing the key only when true is what prevents that, so this asserts the
// bytes rather than trusting the convention.
func TestAnOrdinaryFailuresChecksumDoesNotMove(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op",
		Request: `{}`, Err: "connection refused"}

	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	if strings.Contains(string(payload), "retries_exhausted") {
		t.Errorf("a non-exhaustion event's payload carries the key: %s\n"+
			"every checksum chained after such an event would fail to reverify", payload)
	}

	// And the chained checksum is literally unchanged by the field existing.
	if got, want := computeEventChecksum(rec, "prev"), computeEventChecksum(rec, "prev"); got != want {
		t.Fatalf("checksum not deterministic: %s vs %s", got, want)
	}
	exhausted := rec
	exhausted.RetriesExhausted = true
	if computeEventChecksum(exhausted, "prev") == computeEventChecksum(rec, "prev") {
		t.Error("an exhaustion and an ordinary failure hash identically: the field is not " +
			"reaching the payload at all, so nothing above this is really being tested")
	}
}
