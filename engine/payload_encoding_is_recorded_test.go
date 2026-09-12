package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestDecodePayloadUsesTheRecordedEncoding is cleat#1319.
//
// tryDecodeBase64 fell back to the raw string when decoding FAILED, which is
// the wrong question: ordinary text decodes fine. base64.StdEncoding accepts
// any string whose length is a multiple of 4 and whose bytes are all in
// [A-Za-z0-9+/] with valid padding, so EVERY four-character alphanumeric string
// decodes and a legacy plaintext value was silently replaced with other bytes.
//
// THE PREDICATE, NOT THE SAMPLE. The six values below are an illustration; the
// rule is what makes this common rather than exotic, and the rule cannot be
// argued with by choosing a different sample.
func TestDecodePayloadUsesTheRecordedEncoding(t *testing.T) {
	base64Enc := sql.NullInt16{Int16: payloadEncodingBase64, Valid: true}
	plaintext := sql.NullInt16{Int16: payloadEncodingPlaintext, Valid: true}
	unknown := sql.NullInt16{} // NULL: the row predates the column

	t.Run("recorded plaintext is returned verbatim, however base64 it looks", func(t *testing.T) {
		// Every one of these decodes successfully as base64 and is NOT base64.
		for _, s := range []string{"test", "abcd", "1234", "user", "true", "null"} {
			if got := decodePayload(s, plaintext); got != s {
				t.Errorf("decodePayload(%q, plaintext) = %q, want %q -- the column says "+
					"plaintext and the value was decoded anyway", s, got, s)
			}
		}
	})

	t.Run("recorded base64 is decoded", func(t *testing.T) {
		// "test" is the base64 of these bytes; with the column set, the round
		// trip is exact rather than a coin flip.
		if got := decodePayload("aGVsbG8gd29ybGQ=", base64Enc); got != "hello world" {
			t.Errorf("decodePayload = %q, want %q", got, "hello world")
		}
	})

	t.Run("NULL keeps the historical guess, and the guess is still wrong", func(t *testing.T) {
		// This is not an aspiration: it is what a pre-migration row does, and
		// it CANNOT be improved, because the information was never written
		// down. Asserting it pins the boundary of the fix.
		if got := decodePayload("test", unknown); got == "test" {
			t.Error("decodePayload(\"test\", NULL) returned the raw string; the historical " +
				"fallback decodes it, and pretending otherwise would claim this change " +
				"repairs rows it cannot reach")
		}
		// A value that is not valid base64 falls back correctly even at NULL.
		if got := decodePayload("hello!", unknown); got != "hello!" {
			t.Errorf("decodePayload(%q, NULL) = %q, want the raw string", "hello!", got)
		}
	})

	t.Run("empty is empty whatever the column says", func(t *testing.T) {
		for _, enc := range []sql.NullInt16{base64Enc, plaintext, unknown} {
			if got := decodePayload("", enc); got != "" {
				t.Errorf("decodePayload(\"\", %v) = %q, want \"\"", enc, got)
			}
		}
	})
}

// TestARoundTripThroughTheStoreSurvivesABase64LookingPayload is the end-to-end
// half, across every configured dialect.
//
// The unit test above proves decodePayload consults the column. This proves the
// column is actually WRITTEN and READ BACK — a decoder that consults a column
// nothing populates is the same defect with more steps, and NULL degrades to
// the old guess, so a missing write would look like success on any value that
// is not valid base64.
//
// The payload is chosen to be exactly the failing case: valid base64, and not
// what it decodes to.
func TestARoundTripThroughTheStoreSurvivesABase64LookingPayload(t *testing.T) {
	const payload = "test" // decodes to "\xb5\xeb-" if the encoding is guessed

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := t.Context()

			wf := newIntentWorkflow(t, ctx, store, "payload-encoding")
			rec := EventRecord{
				Step: 0, EventType: EventTypeCall,
				Service: "svc", Op: "op",
				Request: payload, Response: payload,
			}
			if err := store.AppendEventHistory(ctx, wf, rec); err != nil {
				t.Fatalf("AppendEventHistory: %v", err)
			}

			got, err := store.LoadEventHistory(ctx, wf)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("loaded %d events, want 1", len(got))
			}
			if got[0].Request != payload {
				t.Errorf("Request round-tripped as %q, want %q.\n\n"+
					"%q is valid base64, so a reader that guesses decodes it to other "+
					"bytes. Getting it back intact means the encoding was recorded on "+
					"write and consulted on read.", got[0].Request, payload, payload)
			}
			if got[0].Response != payload {
				t.Errorf("Response round-tripped as %q, want %q", got[0].Response, payload)
			}
		})
	}
}

// TestEveryEventHistoryReadConsultsTheRecordedEncoding is the completeness
// half.
//
// Nine read paths across four files load request/response. One left on
// tryDecodeBase64 keeps guessing, on whichever dialect and query it serves —
// and because NULL falls back to exactly that guess, the difference is
// invisible for every value that is not valid base64.
func TestEveryEventHistoryReadConsultsTheRecordedEncoding(t *testing.T) {
	var offenders []string
	for _, f := range []string{
		"store_events.go", "store_event_stream.go", "mysql_events.go", "mssql_events.go",
	} {
		src := repoFile(t, "engine/"+f)
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "tryDecodeBase64(") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: ", f, i+1)+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d read path(s) still infer the encoding instead of reading it:\n  %s\n\n"+
			"Use decodePayload with the scanned payload_encoding column.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestACallIntentPreservesABase64LookingRequest is cleat#1319's live case, and
// the only one that was reachable in current code.
//
// Every other writer populates the `payload` column, whose JSON carries
// request_b64/response_b64 explicitly and whose populateFromPayload runs AFTER
// the scanned columns — so those rows are shadowed and were never at risk.
//
// A call intent is not shadowed. It stores rec.Request RAW and writes no
// payload, so until CompleteCallIntent fills the payload in, the request column
// is the only copy of the value — and the reader used to guess its encoding.
// Measured before the fix, identically on all three dialects:
//
//	"true" -> "\xb6\xbb\x9e"    "null" -> "\x9e\xe9e"    "1234" -> "\xd7m\xf8"
//
// These rows are what the ambiguity resolver reads after a crash, which is when
// a wrong request body costs the most.
func TestACallIntentPreservesABase64LookingRequest(t *testing.T) {
	// JSON objects were never at risk: '{' and '"' are not in the base64
	// alphabet, so they never decoded. The scalars are the reachable case, and
	// the object is the control — without it, "nothing was corrupted" would
	// also be satisfied by a reader that stopped decoding entirely.
	cases := []string{"true", "null", "1234", `{"a":1}`}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := t.Context()
			st := intentStoreOf(t, store)
			wf := newIntentWorkflow(t, ctx, store, "intent-encoding")

			for i, req := range cases {
				rec := EventRecord{
					Step: i, EventType: EventTypeCall,
					Service: "svc", Op: "op", Request: req,
				}
				if err := st.WriteCallIntent(ctx, wf, rec, "", 0); err != nil {
					t.Fatalf("WriteCallIntent(%q): %v", req, err)
				}
			}

			got, err := store.LoadEventHistory(ctx, wf)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(got) != len(cases) {
				t.Fatalf("loaded %d intents, want %d", len(got), len(cases))
			}
			for i, want := range cases {
				if got[i].Request != want {
					t.Errorf("intent request round-tripped as %q, want %q.\n\n"+
						"An intent row has no payload column, so the request column is the "+
						"only copy. Guessing its encoding decodes %q to other bytes, and the "+
						"ambiguity resolver reads these rows after a crash.",
						got[i].Request, want, want)
				}
			}
		})
	}
}
