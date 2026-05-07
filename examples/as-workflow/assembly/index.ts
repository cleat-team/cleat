// AssemblyScript workflow example for cleat durable execution.
//
// Compiled to WASM with: npx asc assembly/index.ts --runtime stub --optimize
//   --initialMemory 170 -o dist/workflow.wasm
//   --transform ./node_modules/@cleat/transform/index.js
//
// Uses direct ABI exports (no @cleatEntry decorator).  Input/output JSON
// is parsed manually via string helpers.
//
// NOTE: Scoped imports like `@cleat/sdk` do not resolve correctly in AS 0.27.32
// due to a module resolution limitation with `@scope/name` packages in the
// runtime library path. Use relative imports as the standard pattern.
//
// ABI export signature:
//   (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) -> i64

import {
  HostCalls, readString, writeString,
  encodeExportResult,
  CleatCallOutcome,
} from "../../../packages/cleat-as/assembly/index";

// ---------------------------------------------------------------------------
// Manual JSON helpers
//
// AS 0.27.32 with --runtime stub has no JSON.parse<T>().  We implement minimal
// field extraction for our known flat JSON objects.
// ---------------------------------------------------------------------------

/** Extract a string field value from a flat JSON object. */
function extractStringField(json: string, field: string): string {
  let searchKey: string = '"' + field + '":"';
  let keyIdx: i32 = indexOf(json, searchKey);
  if (keyIdx < 0) return "";
  let start: i32 = keyIdx + searchKey.length;
  let end: i32 = indexOf(json, '"', start);
  if (end < 0) return "";
  return json.substring(start, end);
}

/** Extract an i64 integer field value from a flat JSON object. */
function extractI64Field(json: string, field: string): i64 {
  let searchKey: string = '"' + field + '":';
  let keyIdx: i32 = indexOf(json, searchKey);
  if (keyIdx < 0) return 0;
  let start: i32 = keyIdx + searchKey.length;
  while (start < json.length && json.charAt(start) == ' ') start++;
  let end: i32 = start;
  while (end < json.length && isDigit(json.charAt(end))) end++;
  if (end <= start) return 0;
  let numStr: string = json.substring(start, end);
  return parseI64(numStr);
}

/** Extract a raw JSON array value for a field (preserving array structure). */
function extractRawArray(json: string, field: string): string {
  let searchKey: string = '"' + field + '":[';
  let keyIdx: i32 = indexOf(json, searchKey);
  if (keyIdx < 0) return "";
  let start: i32 = keyIdx + searchKey.length - 1; // include '['
  let depth: i32 = 1;
  let pos: i32 = start + 1;
  while (pos < json.length && depth > 0) {
    if (json.charAt(pos) == '[') depth++;
    if (json.charAt(pos) == ']') depth--;
    pos++;
  }
  return json.substring(start, pos);
}

/** Simple indexOf for strings with optional start position. */
function indexOf(s: string, search: string, start: i32 = 0): i32 {
  if (search.length === 0) return start;
  if (s.length < search.length) return -1;
  let max: i32 = s.length - search.length;
  for (let i: i32 = start; i <= max; i++) {
    let found: bool = true;
    for (let j: i32 = 0; j < search.length; j++) {
      if (s.charAt(i + j) !== search.charAt(j)) {
        found = false;
        break;
      }
    }
    if (found) return i;
  }
  return -1;
}

function isDigit(c: string): bool {
  return c >= "0" && c <= "9";
}

/** Parse a decimal string into an i64 (ASCII digits only). */
function parseI64(s: string): i64 {
  let result: i64 = 0;
  let negative: bool = false;
  let i: i32 = 0;
  if (s.length > 0 && s.charAt(0) == "-") {
    negative = true;
    i = 1;
  }
  while (i < s.length) {
    let c: string = s.charAt(i);
    if (c < "0" || c > "9") break;
    result = result * 10 + (c.charCodeAt(0) - 48);
    i++;
  }
  if (negative) result = -result;
  return result;
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

function writeResult(outPtr: usize, maxOutLen: i32, json: string): i64 {
  let written: i32 = writeString(outPtr, maxOutLen, json);
  return encodeExportResult(0, written);
}

function writeError(outPtr: usize, maxOutLen: i32, message: string): i64 {
  let errBody: string = '{"error":"' + message + '"}';
  let written: i32 = writeString(outPtr, maxOutLen, errBody);
  return encodeExportResult(1, written);
}

function safeStr(s: string | null): string {
  if (s === null) return "unknown error";
  return s;
}

// ---------------------------------------------------------------------------
// Workflow: place_order
//
// Saga-like orchestration with compensation:
//   1. Reserve inventory
//   2. Charge payment  (on failure: release inventory)
//   3. Create shipment (on failure: refund + release inventory)
//   4. Notify customer (best-effort, no compensation needed)
// ---------------------------------------------------------------------------

export function place_order(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h: HostCalls = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let userID: string = extractStringField(argsJson, "userID");
  let itemsJson: string = extractRawArray(argsJson, "items");

  if (itemsJson.length <= 2) { // "[]" or empty
    return writeError(outPtr, maxOutLen, "cart is empty");
  }

  // --- Step 1: Reserve inventory -------------------------------------------

  let reserveReq: string = '{"user_id":"' + userID + '","items":' + itemsJson + '}';
  let reserveResult: CleatCallOutcome = h.cleatCall("inventory", "Reserve", reserveReq);
  if (reserveResult.isError) {
    return writeError(outPtr, maxOutLen, "inventory reserve failed: " + safeStr(reserveResult.error));
  }
  let reservationID: string = extractStringField(reserveResult.response, "reservationID");
  let totalCents: i64 = extractI64Field(reserveResult.response, "totalCents");

  // --- Step 2: Charge payment ----------------------------------------------

  let chargeReq: string = '{"user_id":"' + userID + '","amount_cents":' + totalCents.toString() + '}';
  let chargeResult: CleatCallOutcome = h.cleatCall("payments", "Charge", chargeReq);
  if (chargeResult.isError) {
    // Compensate: release the reserved inventory.
    h.cleatCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    return writeError(outPtr, maxOutLen, "payment failed: " + safeStr(chargeResult.error));
  }
  let chargeID: string = extractStringField(chargeResult.response, "chargeID");

  // --- Step 3: Create shipment --------------------------------------------

  let shipReq: string = '{"reservation_id":"' + reservationID + '","charge_id":"' + chargeID + '"}';
  let shipResult: CleatCallOutcome = h.cleatCall("shipping", "CreateShipment", shipReq);
  if (shipResult.isError) {
    // Compensate: refund the payment and release inventory.
    h.cleatCall("payments", "Refund", '{"charge_id":"' + chargeID + '"}');
    h.cleatCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    return writeError(outPtr, maxOutLen, "shipping failed: " + safeStr(shipResult.error));
  }

  // --- Step 4: Notify customer (best-effort) -------------------------------

  h.cleatCall("notifications", "SendEmail", '{"user_id":"' + userID + '","message":"Order shipped"}');

  let result: string = '{"status":"shipped","reservation_id":"' + reservationID + '"}';
  return writeResult(outPtr, maxOutLen, result);
}

// ---------------------------------------------------------------------------
// Workflow: cancel_order
//
// A cancellation-aware entry point that checks cancellation before proceeding.
// ---------------------------------------------------------------------------

export function cancel_order(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h: HostCalls = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let userID: string = extractStringField(argsJson, "userID");
  let itemsJson: string = extractRawArray(argsJson, "items");

  // ---- Check cancellation ----
  let cancelledStatus = h.pollCancellation();
  if (cancelledStatus.cancelled) {
    return writeResult(outPtr, maxOutLen,
      '{"status":"cancelled","reason":"cancelled before processing"}');
  }

  if (itemsJson.length <= 2) {
    return writeError(outPtr, maxOutLen, "cart is empty");
  }

  let reserveReq: string = '{"user_id":"' + userID + '","items":' + itemsJson + '}';
  let reserveResult: CleatCallOutcome = h.cleatCall("inventory", "Reserve", reserveReq);
  if (reserveResult.isError) {
    return writeError(outPtr, maxOutLen, "inventory reserve failed: " + safeStr(reserveResult.error));
  }

  return writeResult(outPtr, maxOutLen, '{"status":"reserved"}');
}
