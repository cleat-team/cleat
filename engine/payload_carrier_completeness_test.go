package engine

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// The database payload is one of three carriers an EventRecord passes through,
// and it was the only one with nothing checking that it carries every field
// the replay path reads back.
//
// The other two do have such a check. FuzzCompactionEquivalence compares every
// exported field of every event through a compaction round trip, against
// compactionExemptFields, and engine/events.go's typed-event conversions are
// covered by TestEvent_RoundTrip. eventRecordToPayload / populateFromPayload
// had per-event-type tests only -- one per defect, written after the defect --
// so a field that no arm wrote was invisible until something noticed the
// behaviour it broke.
//
// Three had been missed by the time this file was written, and each was found
// by reading a comment in compaction.go that explained why *compaction*
// carries the field:
//
//	StreamFinish / StreamChunkIndex  a recorded plugin stream error replayed
//	                                 as a success (3.96)
//	NewVersion                       a versioned continue-as-new replayed from
//	                                 the database restarted as version 0,
//	                                 which is "current version" -- so it could
//	                                 run the wrong code
//	StateKeys                        ListState replayed from the database
//	                                 handed the guest no keys, and reported
//	                                 success doing it
//
// What this test checks, and what it does not. It asserts that every
// non-exempt field survives the payload round trip for *at least one* event
// type. It does not assert that the field survives for the event type that
// actually produces it -- deriving that association needs a per-type table,
// and the table would be the thing going stale. The narrower claim is still
// the one that catches the defect above: a field written into no arm at all
// survives for zero event types. The real-store test below covers the
// specific pairs that were wrong.

// payloadExemptFields lists EventRecord fields allowed to NOT survive an
// eventRecordToPayload / populateFromPayload round trip, each with the reason.
// Modelled on compactionExemptFields, which audits the same struct against a
// different carrier; where a field is exempt from both, the reason here says so
// rather than restating it.
//
// Re-derive the list of fields to audit with:
//
//	go doc -all github.com/cleat-team/cleat/engine.EventRecord
//
// and check any new one against the two switches in engine/store_events.go
// before adding it here. A field belongs in this map only if the payload
// genuinely cannot or need not carry it -- not because adding the key is
// inconvenient. Every field below is column-carried, derived, or dead.
var payloadExemptFields = map[string]string{
	"Step": "column: event_history.step, in the INSERT and the SELECT " +
		"(store_event_write.go, store_events.go). Asserted by " +
		"TestThePayloadExemptFieldsAreCarriedByColumnsInstead.",
	"EventType": "column: event_history.event_type. Also the switch key both " +
		"halves of the round trip dispatch on, so it is an input to the " +
		"carrier rather than cargo. Asserted below.",
	"TimestampMs": "derived from a column, not carried: the SELECT computes " +
		"it as EXTRACT(EPOCH FROM created_at)*1000 rather than reading a " +
		"stored millisecond value. Asserted below.",
	"CreatedAt": "column: event_history.created_at, but only PostgreSQL's " +
		"LoadEventHistory actually returns it. MySQL's SELECT derives " +
		"timestamp_ms from created_at without reading the column, and SQL " +
		"Server's reads it, derives TimestampMs, and drops the value -- so " +
		"EventRecord.CreatedAt is the zero time on two of three dialects. " +
		"Measured 2026-09-03; see IMPROVEMENT-PLAN 3.97. Not fixed here " +
		"because it is a different carrier from this file's subject and a " +
		"wider one: MySQL alone has three read paths (LoadEventHistory, " +
		"LoadEventHistoryPaginated, StreamEventHistory) and the fix belongs " +
		"with a test that holds all of them to the same answer, not with two " +
		"one-line patches that would make this file green over the other four " +
		"paths. Documented on EventRecord as being for admin timeline " +
		"visualization and not required for deterministic replay, which is " +
		"why it is a defect and not a data-loss bug.",
	"Pending": "derived read (json:\"-\"): computed at load time from " +
		"intent_at IS NOT NULL AND checksum IS NULL (store_intent.go), never " +
		"itself a stored value, so a payload has nothing to carry. types.go " +
		"explains why it is derived rather than a third column.",
	"Idempotent": "no payload key and no event_history column in any backend. " +
		"The bit does not survive an ordinary restart-replay either, " +
		"independent of this carrier, and replayPluginCall (plugins.go) falls " +
		"back to a live registry lookup for exactly that reason -- so adding " +
		"the key here would change replay behaviour rather than preserve it, " +
		"which is a decision and not a gap. compactionExemptFields carries the " +
		"same reason.",
	"UpdatePayload": "dead field: declared on EventRecord and assigned nowhere " +
		"in the engine. RegisterUpdateHandler (lifecycle.go) sets only " +
		"UpdateHandlerName, and no replay path reads these three. Nothing to " +
		"lose. Re-derive: grep -rn 'UpdatePayload' engine/*.go | grep -v _test",
	"UpdateResponse": "dead field, see UpdatePayload",
	"UpdateError":    "dead field, see UpdatePayload",
}

// allEventTypesForCarrierAudit is every EventType the payload switch has an
// arm for, plus the ones it does not, because a field carried only by a type
// with no arm is exactly as lost as one carried by nothing.
//
// A hardcoded list, and it can go stale: a new EventType added to types.go and
// left out here narrows this test's search rather than failing it.
// TestTheCarrierAuditCoversEveryEventTypeConstant guards that.
var allEventTypesForCarrierAudit = []EventType{
	EventTypeCall, EventTypeAwaitSignals, EventTypeSignalReceived, EventTypeDefer,
	EventTypeChildWorkflow, EventTypeAwaitChild, EventTypeContinueAsNew, EventTypeHeartbeat,
	EventTypeAwaitAllChildren, EventTypePluginCall, EventTypeCreatePromise, EventTypeAwaitPromise,
	EventTypePromiseResolved, EventTypePromiseRejected, EventTypeUpdateHandler,
	EventTypeStateMutation, EventTypeRunDetached, EventTypePluginCallStreamChunk,
	EventTypeDurableLog, EventTypeAcquireLock, EventTypeReleaseLock, EventTypeSideEffect,
	EventTypeScopeAcquired, EventTypeDurableSend, EventTypeDurableScheduleInvoke,
	EventTypeFetch, EventTypePollChild, EventTypeAwaitAnyChild, EventTypeAdminAction,
	EventTypeScheduleCron, EventTypeDeleteCron, EventTypeListCrons,
}

// setProbeValue sets v to a distinguishable non-zero value, reporting false
// for a kind it does not know how to probe.
func setProbeValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		v.SetString("carrier-probe")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	default:
		return false
	}
	return true
}

// payloadCarriers returns, for each exported EventRecord field, the event
// types whose payload round trip preserves a probe value in it.
func payloadCarriers(t *testing.T) map[string][]string {
	t.Helper()
	rt := reflect.TypeOf(EventRecord{})
	carriers := map[string][]string{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		for _, et := range allEventTypesForCarrierAudit {
			rec := EventRecord{Step: 1, EventType: et}
			field := reflect.ValueOf(&rec).Elem().Field(i)
			if !setProbeValue(field) {
				break
			}
			want := field.Interface()
			payload, err := eventRecordToPayload(rec)
			if err != nil {
				t.Fatalf("eventRecordToPayload(%s): %v", et, err)
			}
			back := EventRecord{Step: 1, EventType: et}
			populateFromPayload(&back, payload)
			if reflect.DeepEqual(reflect.ValueOf(&back).Elem().Field(i).Interface(), want) {
				carriers[f.Name] = append(carriers[f.Name], string(et))
			}
		}
	}
	return carriers
}

func TestEveryEventRecordFieldTheDatabasePayloadMustCarryDoesCarry(t *testing.T) {
	carriers := payloadCarriers(t)
	rt := reflect.TypeOf(EventRecord{})
	var uncarried []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, exempt := payloadExemptFields[f.Name]; exempt {
			continue
		}
		if len(carriers[f.Name]) == 0 {
			uncarried = append(uncarried, f.Name)
		}
	}
	if len(uncarried) > 0 {
		sort.Strings(uncarried)
		t.Errorf("EventRecord fields that no event type's payload carries: %v\n"+
			"Each one is silently zero on any history loaded from the database, so a "+
			"replay path that reads it diverges from the fresh run that wrote it. Add "+
			"the key to both switches in engine/store_events.go -- guarded on the field "+
			"being non-zero, so payloads written earlier stay byte-identical and their "+
			"stored checksums still verify -- or add the field to payloadExemptFields "+
			"with the reason it cannot or need not be carried.", uncarried)
	}
}

// The exemptions above all claim the field reaches the database some other way.
// This checks the claim rather than trusting it: without this, a wrong reason
// in payloadExemptFields reads as an audit.
func TestThePayloadExemptFieldsAreCarriedByColumnsInstead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "exempt-columns")

		// A whole number of seconds, because created_at is not guaranteed to
		// keep sub-second precision on every dialect and the SELECT derives
		// TimestampMs back out of it. Fixed rather than time.Now() so the
		// assertion does not depend on a clock.
		const wroteMs int64 = 1756900000000

		if err := store.AppendEventHistory(ctx, wfID, EventRecord{
			Step:        0,
			EventType:   EventTypeDurableLog,
			TimestampMs: wroteMs,
			Message:     "hello",
			LogLevel:    "info",
		}); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}
		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(hist) != 1 {
			t.Fatalf("history has %d events, want 1", len(hist))
		}
		got := hist[0]
		if got.Step != 0 {
			t.Errorf("Step = %d, want 0; payloadExemptFields claims a column carries it", got.Step)
		}
		if got.EventType != EventTypeDurableLog {
			t.Errorf("EventType = %q, want %q; payloadExemptFields claims a column carries it",
				got.EventType, EventTypeDurableLog)
		}
		if got.TimestampMs != wroteMs {
			t.Errorf("TimestampMs = %d, want %d; payloadExemptFields claims the SELECT "+
				"derives it from created_at, and a value that does not come back makes "+
				"replay's Now() fall back to session-start or wall-clock time instead of "+
				"the recorded virtual clock", got.TimestampMs, wroteMs)
		}
		// CreatedAt is deliberately NOT asserted here: it comes back only on
		// PostgreSQL, and asserting it per-dialect in this file would put a
		// dialect-parity claim in a test about the dialect-agnostic payload.
		// payloadExemptFields says what is measured and where it is tracked.
	})
}

// A field carried only by an EventType this file forgot to list is as lost as
// one carried by nothing, and the loss would look like a pass.
func TestTheCarrierAuditCoversEveryEventTypeConstant(t *testing.T) {
	listed := map[EventType]bool{}
	for _, et := range allEventTypesForCarrierAudit {
		if listed[et] {
			t.Errorf("EventType %q listed twice in allEventTypesForCarrierAudit", et)
		}
		listed[et] = true
	}
	// eventTypeToCode is compaction's own table of every event type it knows
	// how to compact, maintained by a different concern in a different file --
	// so using it as the reference here means a new EventType has to be
	// forgotten in two places at once to narrow this audit silently.
	for et := range eventTypeToCode {
		if !listed[et] {
			t.Errorf("EventType %q is in eventTypeToCode but not in "+
				"allEventTypesForCarrierAudit, so no field is checked against its "+
				"payload arm. Add it to the list.", et)
		}
	}
}

// The four fields this change added, through the real store rather than the
// encoder, which is what rules out "the payload drops it but a column saves
// it". Before the change NewVersion and StateKeys both failed here on
// PostgreSQL and MySQL alike.
func TestTheFieldsTheReplayPathReadsSurviveTheStore(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "carrier-fields")

		events := []EventRecord{{
			Step:       0,
			EventType:  EventTypeContinueAsNew,
			NewInput:   `{"n":1}`,
			NewVersion: 3,
		}, {
			Step:      1,
			EventType: EventTypeStateMutation,
			StateKey:  "user:",
			StateKeys: `["user:a","user:b"]`,
			StateOp:   "list",
		}, {
			Step:              2,
			EventType:         EventTypeChildWorkflow,
			ChildName:         "child",
			RunID:             "run-1",
			ParentWorkflowID:  "parent-1",
			ParentClosePolicy: "TERMINATE",
		}}
		for _, rec := range events {
			if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
				t.Fatalf("AppendEventHistory step %d: %v", rec.Step, err)
			}
		}

		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(hist) != len(events) {
			t.Fatalf("history has %d events, want %d", len(hist), len(events))
		}
		if hist[0].NewVersion != 3 {
			t.Errorf("NewVersion = %d, want 3; a versioned continue-as-new replayed from "+
				"the database restarts as version 0, which is \"current version\", so it "+
				"can run different code than the run that recorded it", hist[0].NewVersion)
		}
		if hist[1].StateKeys != `["user:a","user:b"]` {
			t.Errorf("StateKeys = %q, want the two keys; ListState's replay path writes "+
				"this to the guest verbatim, and an empty value is handed over as a "+
				"success rather than as a divergence", hist[1].StateKeys)
		}
		if hist[2].ParentWorkflowID != "parent-1" {
			t.Errorf("ParentWorkflowID = %q, want parent-1", hist[2].ParentWorkflowID)
		}
		if hist[2].ParentClosePolicy != "TERMINATE" {
			t.Errorf("ParentClosePolicy = %q, want TERMINATE", hist[2].ParentClosePolicy)
		}
	})
}

// The four keys are written only when their field is non-zero, which is what
// makes the change safe for rows already in event_history: a record whose new
// fields are zero -- every record written before this change -- produces a
// byte-identical payload and still verifies against its stored checksum.
//
// The literals were measured against the encoder WITHOUT this change (git
// stash the file, run, unstash) rather than copied from what the new code
// prints, which would pin whatever it happens to do.
func TestRecordsWrittenBeforeTheseKeysStillMatchTheirStoredChecksums(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  EventRecord
		want string
	}{{
		name: "continue_as_new without a version",
		rec:  EventRecord{Step: 2, EventType: EventTypeContinueAsNew, NewInput: `{"n":1}`},
		want: "247d7244f1bd9ced",
	}, {
		name: "state_mutation without keys",
		rec:  EventRecord{Step: 3, EventType: EventTypeStateMutation, StateKey: "user:", StateOp: "list"},
		want: "10896b03b39f30dd",
	}, {
		name: "child_workflow without parent fields",
		rec:  EventRecord{Step: 4, EventType: EventTypeChildWorkflow, ChildName: "child", RunID: "run-1"},
		want: "a46558e05d0e939c",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeEventChecksum(tc.rec, ""); got != tc.want {
				t.Errorf("checksum = %s, want %s -- every event of this type already in "+
					"event_history now fails verification", got, tc.want)
			}
		})
	}
}

// The other direction, and the one a "only when set" guard can get wrong
// without anything noticing: a key written outside the checksummed payload
// would leave the checksum unchanged, and the compatibility test above would
// still pass. Setting the field must move the checksum.
func TestSettingTheNewFieldsChangesTheChecksum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		without EventRecord
		with    EventRecord
	}{{
		name:    "NewVersion",
		without: EventRecord{Step: 2, EventType: EventTypeContinueAsNew, NewInput: `{"n":1}`},
		with:    EventRecord{Step: 2, EventType: EventTypeContinueAsNew, NewInput: `{"n":1}`, NewVersion: 3},
	}, {
		name:    "StateKeys",
		without: EventRecord{Step: 3, EventType: EventTypeStateMutation, StateKey: "user:", StateOp: "list"},
		with:    EventRecord{Step: 3, EventType: EventTypeStateMutation, StateKey: "user:", StateOp: "list", StateKeys: `["user:a"]`},
	}, {
		name:    "ParentWorkflowID",
		without: EventRecord{Step: 4, EventType: EventTypeChildWorkflow, ChildName: "child", RunID: "run-1"},
		with:    EventRecord{Step: 4, EventType: EventTypeChildWorkflow, ChildName: "child", RunID: "run-1", ParentWorkflowID: "parent-1"},
	}, {
		name:    "ParentClosePolicy",
		without: EventRecord{Step: 4, EventType: EventTypeChildWorkflow, ChildName: "child", RunID: "run-1"},
		with:    EventRecord{Step: 4, EventType: EventTypeChildWorkflow, ChildName: "child", RunID: "run-1", ParentClosePolicy: "TERMINATE"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			bare := computeEventChecksum(tc.without, "")
			set := computeEventChecksum(tc.with, "")
			if bare == set {
				t.Errorf("checksum is %s with and without %s set, so the key is not inside "+
					"the payload the checksum covers -- the round trip may work while the "+
					"stored row carries nothing", bare, tc.name)
			}
		})
	}
}
