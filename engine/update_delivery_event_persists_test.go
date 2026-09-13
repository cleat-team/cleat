package engine

import (
	"context"
	"strings"
	"testing"
)

// TestAnUpdateDeliveryEventPersists is the test whose absence let #914 ship.
//
// # What it covers that nothing did
//
// DurablePollUpdate records an update_received event carrying
// updateRequestKey(...) as UpdateRequestID, and store_events.go puts that into
// event_history.payload, which is JSONB on PostgreSQL. The key joined its two
// halves with a literal NUL.
//
// PostgreSQL does not reject the raw byte -- it never sees one. json.Marshal
// escapes a NUL to the six characters \u0000, so the engine sends valid JSON
// TEXT, and PostgreSQL rejects that escape because jsonb cannot represent the
// codepoint even escaped:
//
//	pq: unsupported Unicode escape sequence (22P05)
//
// So finalizing the segment failed for EVERY update that reached a dispatch
// point -- and it failed AFTER runUpdate had settled the caller's promise, so
// the caller was told "resolved" and then the workflow failed and the handler's
// state was discarded.
//
// Every test of this path used a fake store. A Go string holds a NUL happily;
// only a database objects. This one writes the event.
//
// # Why all three dialects, and what they actually do
//
// Measured 2026-09-07, sending what json.Marshal really produces --
// {"k":"a\u0000b"} -- and not a raw byte, which is a different question with a
// different answer:
//
//	postgres   jsonb, a parsed representation    ERROR: 22P05
//	mysql      JSON, permits the codepoint       accepted
//	mssql      NVARCHAR + ISJSON, a text check   accepted, ISJSON = 1
//
// The escape is well-formed JSON text, so a validator that checks TEXT passes
// it and a type that must REPRESENT the value cannot.
//
// Only PostgreSQL fails, so a single-dialect test on MySQL would have passed
// while the primary backend was broken -- and the two that accept it were
// silently storing a NUL in a column the third validates. Running all three is
// what makes the divergence visible rather than a coincidence of which dialect
// someone happened to test.
func TestAnUpdateDeliveryEventPersists(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			wfID := seedWorkflowForLock(t, store)

			key := updateRequestKey(UpdateRequestInfo{
				UpdateName: "bump",
				PromiseID:  "01234567-89ab-cdef-0123-456789abcdef",
			})

			rec := EventRecord{
				Step:              0,
				EventType:         EventTypeUpdateReceived,
				UpdateHandlerName: "bump",
				UpdatePayload:     `{"n":1}`,
				UpdateRequestID:   key,
			}
			if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
				t.Fatalf("persisting the update delivery event failed: %v\n\n"+
					"The event carries updateRequestKey(...) as UpdateRequestID and "+
					"store_events.go writes it into event_history.payload. A key containing a "+
					"NUL is refused by PostgreSQL with 22P05 -- and by then runUpdate has "+
					"already settled the caller's promise, so the caller sees success for work "+
					"that is about to be thrown away. That is #914.", err)
			}

			// It must come back, and come back decodable: a write that stored
			// something unreadable is not a working write.
			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			var got *EventRecord
			for i := range hist {
				if hist[i].EventType == EventTypeUpdateReceived {
					got = &hist[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("the update_received event is not in the reloaded history (%d events)", len(hist))
			}
			if strings.ContainsRune(got.UpdateRequestID, 0) {
				t.Errorf("the persisted request key contains a NUL: %q", got.UpdateRequestID)
			}

			// And the round trip the guest depends on: replay hands this key
			// back to DurableCompleteUpdate, which splits it to address the row
			// and the promise. A key that survives the database but decodes to
			// the wrong halves settles the wrong thing.
			name, _, promise := splitUpdateRequestKey(got.UpdateRequestID)
			if name != "bump" || promise != "01234567-89ab-cdef-0123-456789abcdef" {
				t.Errorf("the key round-tripped through the database as (%q, %q), want "+
					"(bump, 01234567-89ab-cdef-0123-456789abcdef).\n\n"+
					"DurableCompleteUpdate splits this to find the row and the promise, so "+
					"wrong halves settle the wrong request.", name, promise)
			}
		})
	}
}
