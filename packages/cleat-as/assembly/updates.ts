/**
 * Workflow updates: a request/reply call into a running workflow that can both
 * change its state and return a value to the caller.
 *
 * # Why dispatch happens where it does
 *
 * An update handler is a closure in guest memory. Only guest code can call it,
 * so an arriving update cannot interrupt the workflow -- something in the guest
 * has to ask, and `dispatchUpdates` is the asking.
 *
 * It has to ask at a fixed PROGRAM POSITION rather than a moment in time,
 * because replay re-executes the guest and matches host calls against the
 * recorded history in order. The SDK therefore dispatches immediately before
 * each suspension, and exposes this for workflows that want more. See
 * engine/updater.go for the host side and why delivery is an event.
 *
 * The consequence, stated rather than hidden: an update is handled at the next
 * dispatch point, not the instant it arrives.
 */

import { JsonParser, JsonVal, TYPE_OBJECT, TYPE_STRING } from "./json";

/**
 * A handler: receives the payload JSON, returns the result JSON.
 *
 * AssemblyScript has no closures over local state, so a handler is a plain
 * function reference. A workflow that needs to mutate state does it through
 * module-level variables -- which is exactly what replay reconstructs, since
 * the handler re-runs on every replay.
 */
export type UpdateHandler = (payload: string) => string;

/**
 * A validator: returns "" to accept, or a message to refuse.
 *
 * It runs first and must be read-only, so a refusal costs nothing beyond the
 * completion -- no state change, no durable work. That is the half of the API
 * that makes an update different from a signal.
 */
export type UpdateValidator = (payload: string) => string;

/** A decoded delivery envelope. */
export class UpdateDelivery {
  constructor(
    public readonly name: string,
    public readonly payload: string,
    public readonly requestId: string,
  ) {}
}

/**
 * Decode the envelope `cleat_poll_update` writes.
 *
 * Returns null when the envelope is not the shape the host writes. That is not
 * a caller error -- the host writes it -- so a decode failure means stop rather
 * than skip: a delivery that decodes wrongly once will decode wrongly again,
 * and continuing would spin.
 *
 * The check is on SHAPE, not on a substring: `getString` returns "" for both
 * "absent" and "present but not a string", so the two have to be told apart by
 * the parsed type. The same reasoning as signal-envelope.ts.
 */
export function decodeUpdateDelivery(json: string): UpdateDelivery | null {
  if (json.length === 0) return null;

  let parser = new JsonParser();
  let val = parser.parse(json);
  if (val === null) return null;

  let obj = <JsonVal>val;
  if (obj.type !== TYPE_OBJECT) return null;

  let name: string = "";
  let payload: string = "";
  let requestId: string = "";
  let sawName: bool = false;
  let sawPayload: bool = false;
  let sawRequestId: bool = false;

  for (let i: i32 = 0; i < obj.objKeys.length; i++) {
    let key: string = obj.objKeys[i];
    let v: JsonVal = obj.objValues[i];
    // The value must actually be a string. A key lookup alone cannot tell
    // "absent" from "present but a number", and {"name":42,...} is not a
    // delivery. Same reasoning as signal-envelope.ts.
    if (key == "name") {
      if (v.type !== TYPE_STRING) return null;
      name = v.strVal;
      sawName = true;
    } else if (key == "payload") {
      if (v.type !== TYPE_STRING) return null;
      payload = v.strVal;
      sawPayload = true;
    } else if (key == "request_id") {
      if (v.type !== TYPE_STRING) return null;
      requestId = v.strVal;
      sawRequestId = true;
    }
    // Unknown keys are tolerated rather than refused: the host may add a field
    // to the envelope, and an SDK that rejected the whole delivery for it would
    // stop servicing updates entirely. Contrast signal-envelope.ts, which is
    // strict because there the SHAPE is what distinguishes an envelope from an
    // ordinary payload -- here the caller has already been told this is one.
  }

  if (!sawName || !sawPayload || !sawRequestId) return null;
  // An empty name or request id cannot be acted on: completeUpdate would
  // address nothing, so the caller's promise would never settle and the update
  // would redeliver every segment.
  if (name.length === 0 || requestId.length === 0) return null;

  return new UpdateDelivery(name, payload, requestId);
}
