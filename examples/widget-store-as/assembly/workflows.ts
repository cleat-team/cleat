// AssemblyScript widget-store workflows for the cleat durable execution framework.
//
// Reimplements the DBOS widget-store e-commerce app checkout workflow.
//
// COMPILE-TIME ISSUES (documented in ISSUES.md):
// 1. @cleat/sdk has type errors with AS 0.27.32 (String.UTF8.encodeUnsafe
//    signature changed, usize vs i32 type mismatches)
// 2. json-as has its own compatibility issues with AS 0.27.32 (inline.always)
// 3. --runtime stub does not support try/catch (exceptions)
// 4. No built-in JSON.parse<T>() with --runtime stub
// 5. When using @durableEntry decorator, auto-generated wrappers don't
//    handle SUSPEND_SENTINEL returns from durableSleep
//
// Workarounds:
// - Import from local cleat-runtime.ts (fixed copy of SDK for AS 0.27.32)
// - Manual JSON parsing instead of @json + JSON.parse<T>()
// - Direct ABI exports instead of @durableEntry wrappers
// - No try/catch blocks

import {
  HostCalls, readString, writeString,
  encodeExportResult, SUSPEND_SENTINEL,
  DurableCallOutcome, AwaitSignalsOutcome, DurableResult,
} from "./cleat-runtime";

// ═══════════════════════════════════════════════
// Manual JSON utilities
//
// AS 0.27.32 + --runtime stub has no JSON.parse<T>().
// We implement minimal parsing for our known types.
// ═══════════════════════════════════════════════

/** Extract a string field value from a JSON object string. */
function extractStringField(json: string, field: string): string {
  // Search for: "field":"...value..."
  let searchKey: string = '"' + field + '":"';
  let keyIdx: i32 = indexOf(json, searchKey);
  if (keyIdx < 0) {
    // Try single quotes (some implementations)
    searchKey = '"' + field + '":"';
    keyIdx = indexOf(json, searchKey);
    if (keyIdx < 0) return "";
  }
  let start: i32 = keyIdx + searchKey.length;
  let end: i32 = indexOf(json, '"', start);
  if (end < 0) return "";
  return json.substring(start, end);
}

/** Extract an integer field value from a JSON object string. */
function extractIntField(json: string, field: string): i32 {
  let searchKey: string = '"' + field + '":';
  let keyIdx: i32 = indexOf(json, searchKey);
  if (keyIdx < 0) return 0;
  let start: i32 = keyIdx + searchKey.length;
  // Skip whitespace
  while (start < json.length && json.charAt(start) == ' ') { start++; }
  // Read digits
  let end: i32 = start;
  while (end < json.length && isDigit(json.charAt(end))) { end++; }
  if (end <= start) return 0;
  let numStr: string = json.substring(start, end);
  return parseInt(numStr);
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

/** Parse a string to i32 (simple ASCII digits only). */
function parseInt(s: string): i32 {
  let result: i32 = 0;
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

// ═══════════════════════════════════════════════
// checkoutWorkflow — the main e-commerce workflow
//
// ABI export: (argsPtr, argsLen, outPtr, maxOutLen) => i64
//
// Flow:
//   1. subtractInventory()
//   2. createOrder()
//   3. setQueryState("payment_info")
//   4. awaitSignals("PAYMENT_TOPIC", 120s)
//   5a. Paid: markOrderPaid + childWorkflow(dispatchOrder)
//   5b. Timeout: errorOrder + undoSubtractInventory
// ═══════════════════════════════════════════════

export function checkoutWorkflow(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let product: string = extractStringField(argsJson, "product");
  let quantity: i32 = extractIntField(argsJson, "quantity");

  if (product.length === 0) {
    return writeError(outPtr, maxOutLen, "product is required");
  }

  // ---- Step 1: Subtract inventory ----
  let reserveReq: string = '{"product":"' + product + '","quantity":' + quantity.toString() + '}';
  let reserveResult: DurableCallOutcome = h.durableCall("shop", "subtractInventory", reserveReq);
  if (reserveResult.isError) {
    return writeError(outPtr, maxOutLen, "inventory: " + safeStr(reserveResult.error));
  }

  // ---- Step 2: Create order ----
  let orderResult: DurableCallOutcome = h.durableCall("shop", "createOrder", reserveReq);
  if (orderResult.isError) {
    // Compensate: undo the inventory reservation
    h.durableCall("shop", "undoSubtractInventory", reserveReq);
    return writeError(outPtr, maxOutLen, "order: " + safeStr(orderResult.error));
  }

  // Parse order result (manually, no JSON.parse<T> available)
  let orderId: string = extractStringField(orderResult.response, "order_id");
  let paymentId: string = extractStringField(orderResult.response, "payment_id");

  if (orderId.length === 0) {
    h.durableCall("shop", "undoSubtractInventory", reserveReq);
    return writeError(outPtr, maxOutLen, "invalid order response");
  }

  // ---- Step 3: Expose payment info to external caller ----
  let paymentInfo: string = '{"order_id":"' + orderId + '","payment_id":"' + paymentId + '"}';
  h.setQueryState("payment_info", paymentInfo);

  // ---- Step 4: Wait for payment webhook signal (120s timeout) ----
  let signalResult: AwaitSignalsOutcome = h.awaitSignals('["PAYMENT_TOPIC"]', 120000);
  if (signalResult.isError) {
    return writeError(outPtr, maxOutLen, "signal: " + safeStr(signalResult.error));
  }

  if (signalResult.timedOut) {
    // ---- Step 5b: Payment timeout ----
    h.log("Payment timeout for order " + orderId);
    h.durableCall("shop", "errorOrder", '{"order_id":"' + orderId + '"}');
    h.durableCall("shop", "undoSubtractInventory", reserveReq);

    let timeoutResult: string = '{"order_id":"' + orderId + '","status":"payment_timeout"}';
    h.setQueryState("final_result", timeoutResult);

    let written: i32 = writeString(outPtr, maxOutLen, timeoutResult);
    return encodeExportResult(0, written);
  }

  // ---- Step 5a: Payment received ----
  h.log("Payment received for order " + orderId);
  h.durableCall("shop", "markOrderPaid", signalResult.payload);

  // ---- Step 6: Start dispatchOrder child workflow ----
  let childInput: string = '{"order_id":"' + orderId + '"}';
  let childResult: DurableResult = h.childWorkflow("dispatchOrder", childInput);
  if (childResult.isError) {
    h.log("Warning: dispatch start failed: " + safeStr(childResult.error));
  } else {
    h.log("Started dispatch workflow for order " + orderId);
  }

  // ---- Step 7: Return success ----
  let successResult: string = '{"order_id":"' + orderId + '","status":"paid"}';
  h.setQueryState("final_result", successResult);

  let written: i32 = writeString(outPtr, maxOutLen, successResult);
  return encodeExportResult(0, written);
}

// ═══════════════════════════════════════════════
// dispatchOrder — child workflow for delivery progress
// ═══════════════════════════════════════════════

export function dispatchOrder(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let orderId: string = extractStringField(argsJson, "order_id");

  if (orderId.length === 0) {
    return writeError(outPtr, maxOutLen, "order_id is required");
  }

  // ---- Execute delivery progress loop ----
  return executeDeliveryLoop(h, orderId, outPtr, maxOutLen);
}

/** Delivery loop: update progress every second, 10 iterations. */
function executeDeliveryLoop(
  h: HostCalls, orderId: string, outPtr: usize, maxOutLen: i32
): i64 {
  for (let i: i32 = 0; i < 10; i++) {
    // Sleep 1 second between progress updates
    let shouldSuspend: bool = h.durableSleep(1000);
    if (shouldSuspend) {
      return SUSPEND_SENTINEL;
    }

    // Update delivery progress
    let step: i32 = i + 1;
    let progressReq: string = '{"order_id":"' + orderId + '","progress":"step ' + step.toString() + '/10"}';
    h.durableCall("shop", "updateOrderProgress", progressReq);
  }

  let result: string = '{"order_id":"' + orderId + '","status":"delivered"}';
  let written: i32 = writeString(outPtr, maxOutLen, result);
  return encodeExportResult(0, written);
}

// ═══════════════════════════════════════════════
// Utilities
// ═══════════════════════════════════════════════

function safeStr(s: string | null): string {
  if (s === null) { return "unknown error"; }
  return s;
}

function writeError(outPtr: usize, maxOutLen: i32, message: string): i64 {
  let errBody: string = '{"error":"' + message + '"}';
  let written: i32 = writeString(outPtr, maxOutLen, errBody);
  return encodeExportResult(1, written);
}
