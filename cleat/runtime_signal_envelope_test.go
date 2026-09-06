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
