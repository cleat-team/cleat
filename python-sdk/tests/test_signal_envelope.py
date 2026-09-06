"""Tests for the request/reply signal envelope (IMPROVEMENT-PLAN 3.220)."""

from cleat_sdk.signal_envelope import (
    decode_signal_envelope,
    encode_signal_envelope,
)


def test_round_trips_any_payload():
    """Covers the shapes that broke the splice approach.

    The harnesses used to merge a correlation ID into the payload object,
    which requires the payload to BE a JSON object: for a bare scalar, an
    array or an empty string they sent no correlation ID at all, so the
    receiver had nothing to reply to and the sender waited out its whole
    timeout with no error anywhere.
    """
    payloads = [
        '{"key":"val"}',
        "{}",
        "[1,2,3]",
        '"a bare JSON string"',
        "42",
        "null",
        "",
        "not json at all",
        '{"quote":"he said \\"hi\\"","backslash":"a\\\\b"}',
        "line\nbreak\ttab",
        '{"unicode":"日本語 🎉"}',
        # Envelope-shaped payload: wrapping adds exactly one level.
        '{"cleat_reply_to":"inner-id","payload":"inner"}',
    ]
    for payload in payloads:
        raw = encode_signal_envelope("promise-123", payload)
        unwrapped = decode_signal_envelope(raw)
        assert unwrapped is not None, f"decode(encode({payload!r})) rejected its own envelope"
        reply_to, got = unwrapped
        assert reply_to == "promise-123", f"payload {payload!r}"
        assert got == payload, f"payload {payload!r} did not survive the round trip"


def test_does_not_misread_an_ordinary_payload():
    """Negative control.

    ``await_signals`` offers every inbound payload to the decoder, so an
    over-matching decoder would hand a receiver a truncated payload and an
    address pointing at no promise. The discriminator is the object's shape,
    not the presence of the key anywhere in the text: a substring search
    cannot tell a thing from a mention of the thing.
    """
    not_envelopes = [
        ("empty", ""),
        ("not json", "cleat_reply_to"),
        ("empty object", "{}"),
        ("array", '["cleat_reply_to","payload"]'),
        ("bare string naming the key", '"cleat_reply_to"'),
        ("address key only", '{"cleat_reply_to":"p1"}'),
        ("payload key only", '{"payload":"x"}'),
        ("a third key", '{"cleat_reply_to":"p1","payload":"x","extra":1}'),
        ("empty address", '{"cleat_reply_to":"","payload":"x"}'),
        ("address is not a string", '{"cleat_reply_to":42,"payload":"x"}'),
        ("user data mentioning the key", '{"note":"set cleat_reply_to later","payload":"x"}'),
        ("nested, not top level", '{"meta":{"cleat_reply_to":"p1","payload":"x"}}'),
    ]
    for name, raw in not_envelopes:
        assert decode_signal_envelope(raw) is None, f"{name}: {raw!r} was read as an envelope"


def test_wire_format_matches_the_other_sdks():
    """The Go and Rust SDKs pin this exact byte sequence for the same inputs.

    A Go or Rust workflow can answer a Python one, so the encoders agreeing is
    a correctness requirement rather than a stylistic one -- and cross-language
    interop is exactly what no single-language test run exercises, so a drift
    would otherwise surface as a receiver that cannot reply, in an integration
    suite, long after the change that caused it.

    Python is the one that needs this most: ``json.dumps`` defaults to ``", "``
    and ``": "`` separators, which do NOT match Go's ``encoding/json`` or
    Rust's ``serde_json``. Only the explicit ``separators`` argument in
    encode_signal_envelope makes these bytes come out right.
    """
    raw = encode_signal_envelope("promise-123", '{"key":"val"}')
    assert raw == '{"cleat_reply_to":"promise-123","payload":"{\\"key\\":\\"val\\"}"}'
