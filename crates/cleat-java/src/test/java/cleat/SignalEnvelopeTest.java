package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/** Tests for the request/reply signal envelope (IMPROVEMENT-PLAN 3.220). */
class SignalEnvelopeTest {

    /**
     * Covers the shapes that broke the splice approach. The harnesses used to
     * merge a correlation ID into the payload object, which requires the
     * payload to BE a JSON object: for a bare scalar, an array or an empty
     * string they sent no correlation ID at all, so the receiver had nothing
     * to reply to and the sender waited out its whole timeout.
     */
    @Test
    void roundTripsAnyPayload() {
        String[] payloads = {
            "{\"key\":\"val\"}",
            "{}",
            "[1,2,3]",
            "\"a bare JSON string\"",
            "42",
            "null",
            "",
            "not json at all",
            "{\"quote\":\"he said \\\"hi\\\"\",\"backslash\":\"a\\\\b\"}",
            "line\nbreak\ttab",
            "{\"unicode\":\"日本語 🎉\"}",
            // Envelope-shaped payload: wrapping adds exactly one level.
            "{\"cleat_reply_to\":\"inner-id\",\"payload\":\"inner\"}",
        };
        for (String payload : payloads) {
            String raw = SignalEnvelope.encode("promise-123", payload);
            String[] got = SignalEnvelope.decode(raw);
            assertNotNull(got, "decode(encode(" + payload + ")) rejected its own envelope");
            assertEquals("promise-123", got[0], "payload " + payload);
            assertEquals(payload, got[1], "payload did not survive the round trip: " + payload);
        }
    }

    /**
     * Negative control. awaitSignals offers every inbound payload to the
     * decoder, so an over-matching decoder would hand a receiver a truncated
     * payload and an address pointing at no promise.
     *
     * <p>The discriminator is the object's shape -- exactly two keys, both
     * expected, address non-empty -- not the presence of the key anywhere in
     * the text: a substring search cannot tell a thing from a mention of the
     * thing.
     *
     * <p>This class was written after a falsification found its absence:
     * deleting the size check from {@link SignalEnvelope#decode} left the
     * whole Java suite green, while the same deletion failed in Go, Rust and
     * Python. The gap was in the tests, not the code.
     */
    @Test
    void doesNotMisreadAnOrdinaryPayload() {
        String[][] notEnvelopes = {
            {"empty", ""},
            {"not json", "cleat_reply_to"},
            {"empty object", "{}"},
            {"array", "[\"cleat_reply_to\",\"payload\"]"},
            {"bare string naming the key", "\"cleat_reply_to\""},
            {"address key only", "{\"cleat_reply_to\":\"p1\"}"},
            {"payload key only", "{\"payload\":\"x\"}"},
            {"a third key", "{\"cleat_reply_to\":\"p1\",\"payload\":\"x\",\"extra\":1}"},
            {"empty address", "{\"cleat_reply_to\":\"\",\"payload\":\"x\"}"},
            {"address is not a string", "{\"cleat_reply_to\":42,\"payload\":\"x\"}"},
            {"user data mentioning the key",
                "{\"note\":\"set cleat_reply_to later\",\"payload\":\"x\"}"},
            {"nested, not top level", "{\"meta\":{\"cleat_reply_to\":\"p1\",\"payload\":\"x\"}}"},
        };
        for (String[] tc : notEnvelopes) {
            assertNull(SignalEnvelope.decode(tc[1]),
                tc[0] + ": " + tc[1] + " was read as an envelope");
        }
    }

    /**
     * Pins this SDK's exact output, so a STRUCTURAL drift -- a renamed key, a
     * reordering, a stray space between tokens -- fails here rather than in a
     * cross-language integration nobody runs locally. Go, Rust and Python pin
     * the same literal.
     *
     * <p>It is not a byte-identity guarantee across SDKs. The sample
     * deliberately contains no {@code <}, {@code >} or {@code &}, because
     * those are exactly where the SDKs legitimately differ -- Go HTML-escapes
     * them and this one does not -- so adding one would make this test assert
     * a false equivalence.
     */
    @Test
    void wireFormatMatchesTheOtherSdks() {
        assertEquals(
            "{\"cleat_reply_to\":\"promise-123\",\"payload\":\"{\\\"key\\\":\\\"val\\\"}\"}",
            SignalEnvelope.encode("promise-123", "{\"key\":\"val\"}"));
    }

    /**
     * Every SDK's decoder must accept every other SDK's output. This is what
     * cross-language request/reply actually depends on.
     *
     * <p>The two forms below are the SAME envelope. Go emits the first; this
     * SDK, Rust and Python emit the second. A payload carrying {@code &} -- a
     * query string, say -- takes the first shape from a Go sender and the
     * second from this one.
     */
    @Test
    void decodesWhatTheOtherSdksEncode() {
        // NOTE the backslash counts. A Java literal "\\u003c" is the six
        // characters <, which is what JSON needs; "\\\\u003c" would be a
        // literal backslash followed by u003c and would decode to the wrong
        // thing. The first version of this test had the latter and failed --
        // correctly, and against the test rather than the parser.
        //
        // "<" on its own would be worse still: Java processes unicode
        // escapes BEFORE lexing, so it would become a bare '<' at compile time
        // and the test would silently assert nothing about escape handling.
        String goForm =
            "{\"cleat_reply_to\":\"p1\",\"payload\":\"{\\\"q\\\":\\\"a\\u003cb\\u0026c\\u003ed\\\"}\"}";
        String javaRustPythonForm =
            "{\"cleat_reply_to\":\"p1\",\"payload\":\"{\\\"q\\\":\\\"a<b&c>d\\\"}\"}";
        String wantPayload = "{\"q\":\"a<b&c>d\"}";

        // Without this, the test would still pass if goForm had been written
        // unescaped by mistake -- both would decode fine and nothing about
        // escape handling would be proved. Asserting they DIFFER is what makes
        // this a known-positive rather than two copies of the same input.
        assertNotEquals(goForm, javaRustPythonForm,
            "the two forms must be different encodings of the same envelope");

        for (String[] tc : new String[][] {{"go", goForm}, {"java/rust/python", javaRustPythonForm}}) {
            String[] got = SignalEnvelope.decode(tc[1]);
            assertNotNull(got, tc[0] + " form was not recognised as an envelope");
            assertEquals("p1", got[0], tc[0] + " form");
            assertEquals(wantPayload, got[1], tc[0] + " form");
        }
    }
}
