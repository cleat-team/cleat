/**
 * The request/reply signal envelope.
 *
 * `sendSignalAndWait` creates a durable promise, sends its ID to the target
 * under a reserved key, and awaits it; the receiver answers by resolving that
 * promise. The reply address therefore travels as DATA inside the signal,
 * which is how DBOS and Temporal both handle request/reply -- neither has a
 * primitive for it. See IMPROVEMENT-PLAN 3.220.
 *
 * This must agree on the STRUCTURE of the format with the Go, Rust, Python
 * and Java SDKs -- the two key names, their order, and no spaces between
 * tokens -- because a workflow in any of those languages can answer an
 * AssemblyScript one.
 *
 * They do NOT agree byte for byte. Go's `encoding/json` HTML-escapes `<`, `>`
 * and `&` by default; this SDK and the other three do not, so the same
 * payload leaves a Go sender as `<` and this one as `<`. That is
 * harmless -- both decode to the identical string, because the consumer is a
 * JSON parser -- and the cross-decode test is what guards the property that
 * actually matters.
 */

import { JsonParser, JsonVal, jsonEscape, TYPE_OBJECT, TYPE_STRING } from "./json";

/**
 * The reserved envelope key carrying the reply address.
 *
 * A payload sent with `signalWorkflow` is delivered verbatim and never
 * carries this key, so a receiver can tell a request that wants an answer
 * from a one-way notification: `AwaitSignalsOutcome.replyTo` is empty for the
 * latter.
 */
export const SIGNAL_REPLY_KEY: string = "cleat_reply_to";

/** A decoded envelope: the reply address and the caller's original payload. */
export class SignalEnvelope {
  constructor(
    public readonly replyTo: string,
    public readonly payload: string,
  ) {}
}

/**
 * Wrap a payload with the address to reply to.
 *
 * The caller's payload is carried as a JSON *string* rather than spliced into
 * it as an extra key. Splicing requires the payload to BE a JSON object, and
 * for a bare scalar, an array or an empty string it silently sends no reply
 * address at all -- so the receiver cannot reply and the sender waits out its
 * whole timeout.
 */
export function encodeSignalEnvelope(replyTo: string, payload: string): string {
  return (
    '{"' + SIGNAL_REPLY_KEY + '":"' + jsonEscape(replyTo) +
    '","payload":"' + jsonEscape(payload) + '"}'
  );
}

/**
 * Return the reply address and the caller's payload, or null when `raw` is
 * not an envelope.
 *
 * Requires EXACTLY the two envelope keys and a non-empty address, so an
 * ordinary payload that merely carries a `cleat_reply_to` field among others
 * is not mistaken for one. The discriminator is the object's shape, not the
 * presence of a string anywhere in the text: a substring search cannot tell a
 * thing from a mention of the thing.
 */
export function decodeSignalEnvelope(raw: string): SignalEnvelope | null {
  if (raw.length === 0) return null;

  let parser = new JsonParser();
  let val: JsonVal | null = parser.parse(raw);
  if (val === null) return null;

  let obj = <JsonVal>val;
  if (obj.type !== TYPE_OBJECT) return null;
  if (obj.objKeys.length !== 2) return null;

  let replyTo: string = "";
  let payload: string = "";
  let sawReplyTo: bool = false;
  let sawPayload: bool = false;

  for (let i: i32 = 0; i < obj.objKeys.length; i++) {
    let key: string = obj.objKeys[i];
    let v: JsonVal = obj.objValues[i];
    // The value must actually be a string. A key lookup alone cannot tell
    // "absent" from "present but a number", and {"cleat_reply_to":42,...} is
    // not an envelope.
    if (key == SIGNAL_REPLY_KEY) {
      if (v.type !== TYPE_STRING) return null;
      replyTo = v.strVal;
      sawReplyTo = true;
    } else if (key == "payload") {
      if (v.type !== TYPE_STRING) return null;
      payload = v.strVal;
      sawPayload = true;
    } else {
      return null;
    }
  }

  if (!sawReplyTo || !sawPayload) return null;
  if (replyTo.length === 0) return null;
  return new SignalEnvelope(replyTo, payload);
}
