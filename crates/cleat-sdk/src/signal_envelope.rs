//! The request/reply signal envelope.
//!
//! `send_signal_and_wait` creates a durable promise, sends its ID to the
//! target under a reserved key, and awaits it; the receiver answers by
//! resolving that promise. The reply address therefore travels as DATA inside
//! the signal, which is how DBOS and Temporal both handle request/reply --
//! neither has a primitive for it. See IMPROVEMENT-PLAN 3.220.
//!
//! This module is the single definition of that wire format for Rust. The Go
//! and Python SDKs have counterparts, and all three must agree on the
//! STRUCTURE -- the two key names, their order, and compact separators --
//! because a Go or Python workflow can answer a Rust one.
//!
//! They do NOT agree byte for byte, and a comment here claimed they did until
//! 2026-09-06. Go's `encoding/json` HTML-escapes `<`, `>` and `&` by default;
//! `serde_json` and Python's `json.dumps` do not. So the same payload yields
//! `\u003c` from a Go sender and `<` from this one. That is harmless -- both
//! decode to the identical string, because the consumer is a JSON parser --
//! but it means the pinned literal below guards structure, not bytes, and
//! `decodes_what_the_other_sdks_encode` is the test for the property that
//! actually matters.

use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::BTreeMap;

/// The reserved envelope key carrying the reply address.
///
/// A payload sent with `signal_workflow` is delivered verbatim and never
/// carries this key, so a receiver can tell a request that wants an answer
/// from a one-way notification: `AwaitedSignal::reply_to` is empty for the
/// latter.
pub const SIGNAL_REPLY_KEY: &str = "cleat_reply_to";

#[derive(Serialize, Deserialize)]
struct SignalEnvelope {
    cleat_reply_to: String,
    payload: String,
}

/// Wrap a payload with the address to reply to.
///
/// The caller's payload is carried as a JSON *string* rather than spliced into
/// it as an extra key. Splicing is what the test harnesses did before
/// IMPROVEMENT-PLAN 3.220, and it has two failure modes a wrapper does not: it
/// silently sends no reply address at all when the payload is not a JSON
/// object -- so the receiver cannot reply and the sender waits out its whole
/// timeout -- and it collides with a user key of the same name. A string
/// round-trips any payload unchanged: object, array, bare scalar, or empty.
pub fn encode_signal_envelope(reply_to: &str, payload: &str) -> Result<String, String> {
    serde_json::to_string(&SignalEnvelope {
        cleat_reply_to: reply_to.to_string(),
        payload: payload.to_string(),
    })
    .map_err(|e| format!("encoding signal reply envelope: {}", e))
}

/// Return the reply address and the caller's original payload, or `None` when
/// `raw` is not an envelope.
///
/// This requires EXACTLY the two envelope keys and a non-empty address, so an
/// ordinary payload that merely carries a `cleat_reply_to` field among others
/// is not mistaken for one. The discriminator is the object's shape, not the
/// presence of a string anywhere in the text: a substring search cannot tell a
/// thing from a mention of the thing.
pub fn decode_signal_envelope(raw: &str) -> Option<(String, String)> {
    let fields: BTreeMap<String, Value> = serde_json::from_str(raw).ok()?;
    if fields.len() != 2 {
        return None;
    }
    if !fields.contains_key(SIGNAL_REPLY_KEY) || !fields.contains_key("payload") {
        return None;
    }
    let env: SignalEnvelope = serde_json::from_str(raw).ok()?;
    if env.cleat_reply_to.is_empty() {
        return None;
    }
    Some((env.cleat_reply_to, env.payload))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_any_payload() {
        // The shapes that broke the splice approach: for a bare scalar, an
        // array or an empty string it sent no reply address at all.
        let payloads = [
            r#"{"key":"val"}"#,
            "{}",
            "[1,2,3]",
            r#""a bare JSON string""#,
            "42",
            "null",
            "",
            "not json at all",
            r#"{"quote":"he said \"hi\"","backslash":"a\\b"}"#,
            "line\nbreak\ttab",
            r#"{"unicode":"日本語 🎉"}"#,
            // Envelope-shaped payload: wrapping adds exactly one level.
            r#"{"cleat_reply_to":"inner-id","payload":"inner"}"#,
        ];
        for payload in payloads {
            let raw = encode_signal_envelope("promise-123", payload)
                .unwrap_or_else(|e| panic!("encode({:?}): {}", payload, e));
            let (reply_to, got) = decode_signal_envelope(&raw)
                .unwrap_or_else(|| panic!("decode(encode({:?})) rejected its own envelope", payload));
            assert_eq!(reply_to, "promise-123", "payload {:?}", payload);
            assert_eq!(got, payload, "payload {:?} did not survive the round trip", payload);
        }
    }

    #[test]
    fn does_not_misread_an_ordinary_payload() {
        // Negative control. await_signals offers every inbound payload to the
        // decoder, so over-matching would hand a receiver a truncated payload
        // and an address pointing at no promise.
        let not_envelopes = [
            ("empty", ""),
            ("not json", "cleat_reply_to"),
            ("empty object", "{}"),
            ("array", r#"["cleat_reply_to","payload"]"#),
            ("bare string naming the key", r#""cleat_reply_to""#),
            ("address key only", r#"{"cleat_reply_to":"p1"}"#),
            ("payload key only", r#"{"payload":"x"}"#),
            ("a third key", r#"{"cleat_reply_to":"p1","payload":"x","extra":1}"#),
            ("empty address", r#"{"cleat_reply_to":"","payload":"x"}"#),
            ("address is not a string", r#"{"cleat_reply_to":42,"payload":"x"}"#),
            ("user data mentioning the key", r#"{"note":"set cleat_reply_to later","payload":"x"}"#),
            ("nested, not top level", r#"{"meta":{"cleat_reply_to":"p1","payload":"x"}}"#),
        ];
        for (name, raw) in not_envelopes {
            assert!(
                decode_signal_envelope(raw).is_none(),
                "{}: {:?} was read as an envelope",
                name,
                raw
            );
        }
    }


    /// Every SDK's decoder must accept every other SDK's output. This is the
    /// property cross-language request/reply actually depends on, and it had
    /// no test in any SDK until 2026-09-06 -- only the byte pin, which cannot
    /// see the difference because its sample contains no HTML characters.
    ///
    /// The two forms below are the SAME envelope. Go emits the first, Rust and
    /// Python the second. A payload carrying `&` -- a query string, say --
    /// takes the first shape from a Go sender and the second from this one.
    #[test]
    fn decodes_what_the_other_sdks_encode() {
        let go_form = r#"{"cleat_reply_to":"p1","payload":"{\"q\":\"a\u003cb\u0026c\u003ed\"}"}"#;
        let rust_python_form = r#"{"cleat_reply_to":"p1","payload":"{\"q\":\"a<b&c>d\"}"}"#;
        let want_payload = r#"{"q":"a<b&c>d"}"#;

        // Without this, the test would still pass if the go_form literal had
        // been written unescaped by mistake -- both cases would decode fine
        // and nothing would be proved about escape handling. Asserting they
        // DIFFER is what makes the case a known-positive rather than two
        // copies of the same input.
        assert_ne!(go_form, rust_python_form, "the two forms must be different encodings");

        for (name, raw) in [("go", go_form), ("rust/python", rust_python_form)] {
            let (reply_to, payload) = decode_signal_envelope(raw)
                .unwrap_or_else(|| panic!("{} form was not recognised as an envelope", name));
            assert_eq!(reply_to, "p1", "{} form", name);
            assert_eq!(payload, want_payload, "{} form", name);
        }
    }

    /// Pins this SDK's exact output, which is how a STRUCTURAL drift -- a
    /// renamed key, a reordering, a stray space from non-compact separators --
    /// fails here rather than in a cross-language integration nobody runs
    /// locally.
    ///
    /// The sample deliberately contains no `<`, `>` or `&`, because those are
    /// exactly where the SDKs legitimately differ (see the module doc). Add
    /// one and this test would be asserting a false equivalence.
    #[test]
    fn wire_format_matches_the_go_sdk() {
        let raw = encode_signal_envelope("promise-123", r#"{"key":"val"}"#).unwrap();
        assert_eq!(raw, r#"{"cleat_reply_to":"promise-123","payload":"{\"key\":\"val\"}"}"#);
    }
}
