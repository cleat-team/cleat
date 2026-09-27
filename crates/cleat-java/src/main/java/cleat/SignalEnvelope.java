package cleat;

import java.util.Map;

/**
 * The request/reply signal envelope.
 * <p>
 * {@code sendSignalAndWait} creates a durable promise, sends its ID to the
 * target under a reserved key, and awaits it; the receiver answers by
 * resolving that promise. The reply address therefore travels as DATA inside
 * the signal, which is how DBOS and Temporal both handle request/reply --
 * neither has a primitive for it. See IMPROVEMENT-PLAN 3.220.
 * <p>
 * This must agree on the STRUCTURE of the format with
 * {@code cleat/runtime_signal_envelope.go},
 * {@code crates/cleat-sdk/src/signal_envelope.rs} and
 * {@code python-sdk/cleat_sdk/signal_envelope.py} -- the two key names, their
 * order, and no spaces between tokens -- because a workflow in any of those
 * languages can answer a Java one.
 * <p>
 * They do NOT agree byte for byte. Go's {@code encoding/json} HTML-escapes
 * {@code <}, {@code >} and {@code &} by default; this SDK, serde_json and
 * json.dumps do not, so the same payload leaves a Go sender as
 * {@code \\u003c} and this one as {@code <}. That is harmless -- both decode
 * to the identical string, because the consumer is a JSON parser -- and
 * {@code decodesWhatTheOtherSdksEncode} is the test for the property that
 * actually matters.
 */
public final class SignalEnvelope {

    /**
     * The reserved envelope key carrying the reply address.
     * <p>
     * A payload sent with {@code signalWorkflow} is delivered verbatim and
     * never carries this key, so a receiver can tell a request that wants an
     * answer from a one-way notification: {@code AwaitSignalsResult.replyTo}
     * is empty for the latter.
     */
    public static final String REPLY_KEY = "cleat_reply_to";

    private SignalEnvelope() {
    }

    /**
     * Wrap a payload with the address to reply to.
     * <p>
     * The caller's payload is carried as a JSON <em>string</em> rather than
     * spliced into it as an extra key. Splicing requires the payload to BE a
     * JSON object, and for a bare scalar, an array or an empty string it
     * silently sends no reply address at all -- so the receiver cannot reply
     * and the sender waits out its whole timeout.
     *
     * @param replyTo the reply promise's ID
     * @param payload the caller's payload, passed through unchanged
     * @return the envelope JSON
     */
    public static String encode(String replyTo, String payload) {
        return "{\"" + REPLY_KEY + "\":\"" + JsonHelper.escapeJson(replyTo)
            + "\",\"payload\":\"" + JsonHelper.escapeJson(payload) + "\"}";
    }

    /**
     * Return {@code {replyTo, payload}}, or {@code null} when {@code raw} is
     * not an envelope.
     * <p>
     * Requires EXACTLY the two envelope keys and a non-empty address, so an
     * ordinary payload that merely carries a {@code cleat_reply_to} field
     * among others is not mistaken for one. The discriminator is the object's
     * shape, not the presence of a string anywhere in the text: a substring
     * search cannot tell a thing from a mention of the thing.
     *
     * @param raw the received signal payload
     * @return a two-element array, or {@code null}
     */
    public static String[] decode(String raw) {
        if (raw == null || raw.isEmpty()) {
            return null;
        }
        Map<String, Object> fields;
        try {
            fields = JsonHelper.parseObject(raw);
        } catch (RuntimeException e) {
            // parseObject throws for anything not starting with '{', and can
            // throw while parsing malformed input. A payload that is not an
            // envelope is an ordinary case, not an error.
            return null;
        }
        if (fields == null || fields.size() != 2) {
            return null;
        }
        Object replyTo = fields.get(REPLY_KEY);
        Object payload = fields.get("payload");
        if (!(replyTo instanceof String) || !(payload instanceof String)) {
            return null;
        }
        String address = (String) replyTo;
        if (address.isEmpty()) {
            return null;
        }
        return new String[] {address, (String) payload};
    }
}
