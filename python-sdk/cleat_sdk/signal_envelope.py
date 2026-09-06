"""The request/reply signal envelope.

``send_signal_and_wait`` creates a durable promise, sends its ID to the target
under a reserved key, and awaits it; the receiver answers by resolving that
promise. The reply address therefore travels as DATA inside the signal, which
is how DBOS and Temporal both handle request/reply -- neither has a primitive
for it. See IMPROVEMENT-PLAN 3.220.

This module must agree byte for byte with ``cleat/runtime_signal_envelope.go``
and ``crates/cleat-sdk/src/signal_envelope.rs``, because a Go or Rust workflow
can answer a Python one. All three pin the same literal in a test.
"""

import json

#: The reserved envelope key carrying the reply address.
#:
#: A payload sent with ``signal_workflow`` is delivered verbatim and never
#: carries this key, so a receiver can tell a request that wants an answer from
#: a one-way notification: ``SignalResult.reply_to`` is empty for the latter.
SIGNAL_REPLY_KEY = "cleat_reply_to"


def encode_signal_envelope(reply_to: str, payload: str) -> str:
    """Wrap *payload* with the address to reply to.

    The caller's payload is carried as a JSON *string* rather than spliced into
    it as an extra key. Splicing requires the payload to BE a JSON object, and
    for a bare scalar, an array or an empty string it silently sends no reply
    address at all -- so the receiver cannot reply and the sender waits out its
    whole timeout. A string round-trips any payload unchanged.

    ``separators`` is load-bearing, not style: Python's default is ``", "`` and
    ``": "``, which would emit different bytes from Go's ``encoding/json`` and
    Rust's ``serde_json`` for the same input. Cross-language interop is exactly
    what no single-language test run exercises, so the difference would surface
    as a receiver that cannot reply, in an integration suite, long after the
    change that caused it.
    """
    return json.dumps(
        {SIGNAL_REPLY_KEY: reply_to, "payload": payload},
        separators=(",", ":"),
    )


def decode_signal_envelope(raw: str) -> tuple[str, str] | None:
    """Return ``(reply_to, payload)``, or ``None`` when *raw* is not an envelope.

    Requires EXACTLY the two envelope keys and a non-empty address, so an
    ordinary payload that merely carries a ``cleat_reply_to`` field among
    others is not mistaken for one. The discriminator is the object's shape,
    not the presence of a string anywhere in the text: a substring search
    cannot tell a thing from a mention of the thing.
    """
    try:
        fields = json.loads(raw)
    except (ValueError, TypeError):
        return None
    if not isinstance(fields, dict) or len(fields) != 2:
        return None
    if SIGNAL_REPLY_KEY not in fields or "payload" not in fields:
        return None
    reply_to = fields[SIGNAL_REPLY_KEY]
    payload = fields["payload"]
    if not isinstance(reply_to, str) or not isinstance(payload, str):
        return None
    if reply_to == "":
        return None
    return reply_to, payload
