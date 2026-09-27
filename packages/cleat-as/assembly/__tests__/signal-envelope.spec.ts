/**
 * Tests for the request/reply signal envelope (IMPROVEMENT-PLAN 3.220).
 *
 * `sendSignalAndWait` sends the reply promise's ID inside the signal under a
 * reserved key; the receiver reads it from `AwaitSignalsOutcome.replyTo` and
 * answers by resolving that promise.
 */
import {
  decodeSignalEnvelope,
  encodeSignalEnvelope,
  SignalEnvelope,
} from "../index";

describe("signal envelope", () => {
  // Covers the shapes that broke the splice approach every harness used
  // before 3.220: merging a correlation ID into the payload object requires
  // the payload to BE an object, so for a bare scalar, an array or an empty
  // string it sent no address at all and the sender waited out its timeout.
  it("round-trips any payload", () => {
    let payloads: string[] = [
      '{"key":"val"}',
      "{}",
      "[1,2,3]",
      '"a bare JSON string"',
      "42",
      "null",
      "",
      "not json at all",
      "line\nbreak\ttab",
      '{"unicode":"日本語"}',
      // Envelope-shaped payload: wrapping adds exactly one level.
      '{"cleat_reply_to":"inner-id","payload":"inner"}',
    ];
    for (let i: i32 = 0; i < payloads.length; i++) {
      let payload: string = payloads[i];
      let raw: string = encodeSignalEnvelope("promise-123", payload);
      let got = decodeSignalEnvelope(raw);
      expect<bool>(got !== null).toBe(true, "decode(encode(...)) rejected its own envelope");
      let env = <SignalEnvelope>got;
      expect<string>(env.replyTo).toBe("promise-123");
      expect<string>(env.payload).toBe(payload, "payload did not survive the round trip");
    }
  });

  // Negative control. awaitSignals offers every inbound payload to the
  // decoder, so an over-matching decoder would hand a receiver a truncated
  // payload and an address pointing at no promise. The discriminator is the
  // object's SHAPE -- exactly two keys, both expected, both strings, address
  // non-empty -- not the presence of the key anywhere in the text.
  it("does not misread an ordinary payload", () => {
    let notEnvelopes: string[] = [
      "",
      "cleat_reply_to",
      "{}",
      '["cleat_reply_to","payload"]',
      '"cleat_reply_to"',
      '{"cleat_reply_to":"p1"}',
      '{"payload":"x"}',
      '{"cleat_reply_to":"p1","payload":"x","extra":1}',
      '{"cleat_reply_to":"","payload":"x"}',
      '{"cleat_reply_to":42,"payload":"x"}',
      '{"note":"set cleat_reply_to later","payload":"x"}',
      '{"meta":{"cleat_reply_to":"p1","payload":"x"}}',
    ];
    for (let i: i32 = 0; i < notEnvelopes.length; i++) {
      let got = decodeSignalEnvelope(notEnvelopes[i]);
      expect<bool>(got === null).toBe(true, "a non-envelope was read as an envelope");
    }
  });

  // Pins this SDK's exact output, so a STRUCTURAL drift -- a renamed key, a
  // reordering, a stray space -- fails here rather than in a cross-language
  // integration nobody runs locally. Go, Rust, Python and Java pin the same
  // literal.
  //
  // Not a byte-identity guarantee across SDKs: the sample deliberately has no
  // <, > or &, which is exactly where Go legitimately differs (it HTML-escapes
  // them and this SDK does not), so adding one would assert a false
  // equivalence.
  it("pins the wire format the other SDKs pin", () => {
    expect<string>(encodeSignalEnvelope("promise-123", '{"key":"val"}'))
      .toBe('{"cleat_reply_to":"promise-123","payload":"{\\"key\\":\\"val\\"}"}');
  });

  // Every SDK's decoder must accept every other SDK's output -- the property
  // cross-language request/reply actually depends on. The two forms below are
  // the SAME envelope: Go emits the first, this SDK and the other three the
  // second.
  it("decodes what the other SDKs encode", () => {
    let goForm: string =
      '{"cleat_reply_to":"p1","payload":"{\\"q\\":\\"a\\u003cb\\u0026c\\u003ed\\"}"}';
    let otherForm: string =
      '{"cleat_reply_to":"p1","payload":"{\\"q\\":\\"a<b&c>d\\"}"}';
    let wantPayload: string = '{"q":"a<b&c>d"}';

    // Without this, the test would still pass if goForm had been written
    // unescaped by mistake -- both would decode fine and nothing about escape
    // handling would be proved.
    expect<bool>(goForm != otherForm).toBe(true, "the two forms must differ");

    let fromGo = decodeSignalEnvelope(goForm);
    expect<bool>(fromGo !== null).toBe(true, "go form was not recognised");
    expect<string>((<SignalEnvelope>fromGo).replyTo).toBe("p1");
    expect<string>((<SignalEnvelope>fromGo).payload).toBe(wantPayload, "go form");

    let fromOther = decodeSignalEnvelope(otherForm);
    expect<bool>(fromOther !== null).toBe(true, "other form was not recognised");
    expect<string>((<SignalEnvelope>fromOther).replyTo).toBe("p1");
    expect<string>((<SignalEnvelope>fromOther).payload).toBe(wantPayload, "other form");
  });
});
