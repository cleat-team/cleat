package cleat

import "testing"

// TestSignalEnvelopeRoundTripsAnyPayload covers the shapes that broke the
// approach this replaces. cleattest spliced the correlation ID into the
// payload as an extra key, which requires the payload to BE a JSON object:
// for a bare scalar, an array, or an empty string it silently sent no
// correlation ID at all, so the receiver had nothing to reply to and the
// sender waited out its whole timeout with no error anywhere.
func TestSignalEnvelopeRoundTripsAnyPayload(t *testing.T) {
	payloads := []string{
		`{"key":"val"}`,
		`{}`,
		`[1,2,3]`,
		`"a bare JSON string"`,
		`42`,
		`null`,
		``,
		`not json at all`,
		`{"quote":"he said \"hi\"","backslash":"a\\b"}`,
		"line\nbreak\ttab",
		`{"unicode":"日本語 🎉"}`,
		// A payload that is itself envelope-shaped: wrapping must add
		// exactly one level and unwrapping must remove exactly one.
		`{"cleat_reply_to":"inner-id","payload":"inner"}`,
	}
	for _, payload := range payloads {
		raw, err := encodeSignalEnvelope("promise-123", payload)
		if err != nil {
			t.Fatalf("encode(%q): %v", payload, err)
		}
		replyTo, got, ok := decodeSignalEnvelope(raw)
		if !ok {
			t.Errorf("decode(encode(%q)) did not recognise its own envelope", payload)
			continue
		}
		if replyTo != "promise-123" {
			t.Errorf("payload %q: reply address %q, want %q", payload, replyTo, "promise-123")
		}
		if got != payload {
			t.Errorf("payload %q did not survive the round trip: got %q", payload, got)
		}
	}
}

// TestSignalEnvelopeDoesNotMisreadAnOrdinaryPayload is the negative control.
// Auto-unwrapping in AwaitSignals means every inbound payload is offered to
// the decoder, so a decoder that over-matches would hand a receiver a
// truncated payload and a reply address pointing at no promise.
//
// The discriminator is the object's SHAPE -- exactly two keys, both expected,
// address non-empty -- not the presence of the key anywhere in the text. A
// substring search would accept every case below that mentions the key, which
// is the failure this repo keeps rediscovering: a text match cannot tell a
// thing from a mention of the thing.
func TestSignalEnvelopeDoesNotMisreadAnOrdinaryPayload(t *testing.T) {
	notEnvelopes := []struct{ name, raw string }{
		{"empty", ``},
		{"not json", `cleat_reply_to`},
		{"empty object", `{}`},
		{"array", `["cleat_reply_to","payload"]`},
		{"bare string naming the key", `"cleat_reply_to"`},
		{"address key only", `{"cleat_reply_to":"p1"}`},
		{"payload key only", `{"payload":"x"}`},
		{"a third key", `{"cleat_reply_to":"p1","payload":"x","extra":1}`},
		{"empty address", `{"cleat_reply_to":"","payload":"x"}`},
		{"address is not a string", `{"cleat_reply_to":42,"payload":"x"}`},
		{"user data mentioning the key", `{"note":"set cleat_reply_to later","payload":"x"}`},
		{"nested, not top level", `{"meta":{"cleat_reply_to":"p1","payload":"x"}}`},
	}
	for _, tc := range notEnvelopes {
		if replyTo, payload, ok := decodeSignalEnvelope(tc.raw); ok {
			t.Errorf("%s: %q was read as an envelope (replyTo=%q payload=%q)",
				tc.name, tc.raw, replyTo, payload)
		}
	}
}

// TestUnwrapSignalResultLeavesAOneWaySignalAlone pins the property a receiver
// depends on to tell a request from a notification: SignalWorkflow's payload
// arrives byte-identical with an empty ReplyTo.
func TestUnwrapSignalResultLeavesAOneWaySignalAlone(t *testing.T) {
	in := SignalResult{Name: "sig", Payload: `{"key":"val"}`}
	out := unwrapSignalResult(in)
	if out.ReplyTo != "" {
		t.Errorf("one-way signal gained a reply address %q", out.ReplyTo)
	}
	if out.Payload != in.Payload {
		t.Errorf("one-way payload changed: %q -> %q", in.Payload, out.Payload)
	}
}

func TestUnwrapSignalResultExtractsTheReplyAddress(t *testing.T) {
	raw, err := encodeSignalEnvelope("promise-abc", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := unwrapSignalResult(SignalResult{Name: "sig", Payload: raw})
	if out.ReplyTo != "promise-abc" {
		t.Errorf("reply address %q, want %q", out.ReplyTo, "promise-abc")
	}
	if out.Payload != `{"key":"val"}` {
		t.Errorf("payload %q, want %q", out.Payload, `{"key":"val"}`)
	}
	if out.Name != "sig" {
		t.Errorf("name %q, want %q", out.Name, "sig")
	}
}

// TestSignalEnvelopeWireFormatIsPinned fixes the exact bytes this SDK puts on
// the wire, which is how a STRUCTURAL drift -- a renamed key, a reordering, a
// stray space from non-compact separators -- fails here rather than in a
// cross-language integration nobody runs locally. The Rust and Python SDKs
// pin the same literal.
//
// It is NOT a byte-identity guarantee across SDKs, and the comment here said
// it was until 2026-09-06. encoding/json HTML-escapes `<`, `>` and `&` by
// default; Rust's serde_json and Python's json.dumps do not, so the same
// payload leaves this SDK as \u003c and the others as <. The sample below
// deliberately contains none of those three, because adding one would make
// this test assert a false equivalence. The property that actually matters is
// in TestSignalEnvelopeDecodesWhatTheOtherSDKsEncode.
//
// Field ORDER is part of what is pinned, not incidental: encoding/json emits
// struct fields in declaration order, so reordering the two fields in
// signalEnvelope would change these bytes while every round-trip test above
// kept passing.
func TestSignalEnvelopeWireFormatIsPinned(t *testing.T) {
	got, err := encodeSignalEnvelope("promise-123", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"cleat_reply_to":"promise-123","payload":"{\"key\":\"val\"}"}`
	if got != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", got, want)
	}
}

// TestSignalEnvelopeDecodesWhatTheOtherSDKsEncode is the cross-language
// property request/reply really depends on: every SDK's decoder must accept
// every other SDK's output. It had no test in any SDK until 2026-09-06 --
// only the byte pin, which cannot see the difference because its sample
// contains no HTML characters.
//
// The two forms below are the SAME envelope. This SDK emits the first, Rust
// and Python the second, and a payload carrying `&` -- a query string, say --
// takes the first shape from a Go sender and the second from a Python one.
func TestSignalEnvelopeDecodesWhatTheOtherSDKsEncode(t *testing.T) {
	const (
		goForm         = `{"cleat_reply_to":"p1","payload":"{\"q\":\"a\u003cb\u0026c\u003ed\"}"}`
		rustPythonForm = `{"cleat_reply_to":"p1","payload":"{\"q\":\"a<b&c>d\"}"}`
		wantPayload    = `{"q":"a<b&c>d"}`
	)

	// Without this, the test would still pass if goForm had been written
	// unescaped by mistake -- both cases would decode fine and nothing would
	// be proved about escape handling. Asserting they DIFFER is what makes
	// this a known-positive rather than two copies of the same input.
	if goForm == rustPythonForm {
		t.Fatal("the two forms must be different encodings of the same envelope")
	}

	for _, tc := range []struct{ name, raw string }{
		{"go", goForm},
		{"rust/python", rustPythonForm},
	} {
		replyTo, payload, ok := decodeSignalEnvelope(tc.raw)
		if !ok {
			t.Errorf("%s form was not recognised as an envelope", tc.name)
			continue
		}
		if replyTo != "p1" {
			t.Errorf("%s form: reply address %q, want %q", tc.name, replyTo, "p1")
		}
		if payload != wantPayload {
			t.Errorf("%s form: payload %q, want %q", tc.name, payload, wantPayload)
		}
	}
}
