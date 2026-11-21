package engine

// cleat#4310. Compaction did not carry EventRecord.InDeferPhase or InDeferSegment: CompactedEvent had no field for either, so a record that went through
// extractCompactionStateFrom and back through buildFullHistoryFromCompaction read as "written by the workflow body" whatever it was. Measured with a control: a tail
// record (not compacted) kept both stamps and a compacted one lost both, which is what makes "lost" and not "never carried" the reading.
//
// Everything that reads the stamps was blind to a compacted record: lastStoppableIndex and fastForwardToStop skip drain and segment records by stamp, the redrive
// refusal reads InDeferSegment, and cleat#4285's check for a call recorded after a pending child await had to treat a compacted record's stamp as UNKNOWN.
//
// THE GENERAL ROUND-TRIP CHECK COULD NOT HAVE CAUGHT IT. FuzzCompactionEquivalence compares every non-exempt EventRecord field by reflection, so a dropped stamp would
// fail it, but only for an event that SETS the stamp, and no generated event did. A property check over a field nothing sets agrees with every implementation. So this
// is the row per event type that carries them: every event type compaction has a code for, with each stamp alone and both together, through the real capture and the real
// expansion, compared by the same reflection the property test uses.

import (
	"sort"
	"testing"
)

// cannotBeCompactedYet are the event types compaction has a code for and no capture arm for, so extractCompactionStateFrom refuses a history that holds one (cleat#4316,
// measured: such a history never compacts). Their stamps cannot be checked until that is fixed, and the entry is NAMED so that is not a silent omission: the test below
// fails the moment one of them becomes compactable, which is when the entry must go and the type must be covered like the rest.
var cannotBeCompactedYet = map[EventType]string{
	EventTypeCallAttemptFailed: "cleat#4316: compaction has a code for it and no capture arm, so a history containing one cannot be compacted",
}

func TestCompactionCarriesTheDrainStampsForEveryEventType(t *testing.T) {
	var types []string
	for et := range eventTypeToCode {
		if why, exempt := cannotBeCompactedYet[et]; exempt {
			if _, err := extractCompactionStateFrom(nil, []EventRecord{{Step: 0, EventType: et}}); err == nil {
				t.Errorf("%s is listed in cannotBeCompactedYet (%s) but now compacts: delete the entry so its stamps are checked like every other type", et, why)
			}
			continue
		}
		types = append(types, string(et))
	}
	sort.Strings(types)
	if len(types) < 20 { // a sanity bound on the scan, not a census: it fails on a scan that sees almost nothing
		t.Fatalf("PRECONDITION FAILED: compaction has a code for only %d event types; the scan is not reading the engine", len(types))
	}
	stamps := []struct {
		label          string
		phase, segment bool
	}{{"drain", true, false}, {"segment", false, true}, {"both", true, true}, {"neither", false, false}}
	for _, name := range types {
		et := EventType(name)
		for _, st := range stamps {
			t.Run(name+"/"+st.label, func(t *testing.T) {
				in := EventRecord{Step: 0, EventType: et, TimestampMs: 1_700_000_000_000, InDeferPhase: st.phase, InDeferSegment: st.segment}
				cs, err := extractCompactionStateFrom(nil, []EventRecord{in})
				if err != nil {
					t.Fatalf("capture: %v", err)
				}
				out := buildFullHistoryFromCompaction(nil, cs)
				if len(out) != 1 {
					t.Fatalf("expansion returned %d records, want 1", len(out))
				}
				if out[0].InDeferPhase != st.phase || out[0].InDeferSegment != st.segment {
					t.Errorf("stamps after the round trip: InDeferPhase=%v InDeferSegment=%v, want %v and %v", out[0].InDeferPhase, out[0].InDeferSegment, st.phase, st.segment)
				}
				if d := diffEventFields(in, out[0]); len(d) != 0 {
					t.Errorf("fields that did not survive the round trip: %v", d)
				}
			})
		}
	}
	// CONTROL: a tail record, which is not compacted, keeps its stamps. Without this the rows above cannot tell "compaction loses them" from "this harness cannot carry them".
	tail := EventRecord{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", InDeferPhase: true, InDeferSegment: true}
	cs, err := extractCompactionStateFrom(nil, []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op"}})
	if err != nil {
		t.Fatal(err)
	}
	if full := buildFullHistoryFromCompaction([]EventRecord{tail}, cs); len(full) != 2 || !full[1].InDeferPhase || !full[1].InDeferSegment {
		t.Errorf("CONTROL: a tail record lost its stamps (%+v)", full)
	}
}
