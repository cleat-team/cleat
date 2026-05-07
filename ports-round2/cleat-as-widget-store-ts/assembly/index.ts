// AssemblyScript port of DBOS TypeScript widget-store to cleat.
//
// Provides:
//   checkout_workflow  — Main checkout saga with compensation
//   dispatch_order     — Child workflow for progress-based dispatch
//   cancel_order       — Cancellation-aware checkout variant
//
// Uses manual ABI exports (no @durableEntry decorator) to avoid
// Issue #2 (SUSPEND_SENTINEL handling in the transform).
//
// ABI export signature:
//   (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) -> i64
//
// Compile:
//   npx asc assembly/index.ts --target release --runtime stub
//     --initialMemory 170 -o dist/workflow.wasm

import {
  HostCalls,
  readString,
  writeString,
  encodeExportResult,
  DurableCallOutcome,
  DurableResult,
  AwaitSignalsOutcome,
  CancellationStatus,
  isWorkflowSuspended,
  SUSPEND_SENTINEL,
  Saga,
} from "../../../packages/cleat-as/assembly/index";

// ══════════════════════════════════════════════════════════════════════════════
// JSON helpers
//
// AS 0.27.32 with --runtime stub has no JSON.parse/stringify.
// We use manual field extraction and string construction.
// ══════════════════════════════════════════════════════════════════════════════

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

// ══════════════════════════════════════════════════════════════════════════════
// Output helpers
// ══════════════════════════════════════════════════════════════════════════════

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

// ══════════════════════════════════════════════════════════════════════════════
// Workflow: checkout_workflow
//
// Saga-like checkout with compensation:
//   1. Reserve inventory (durableCall to "inventory" service)
//   2. Create order (durableCall to "orders" service)
//      On failure: release reserved inventory
//   3. Wait for payment signal (awaitSignals, 120s timeout)
//   4a. Payment succeeded:
//       - Mark order paid (durableCall to "orders" service)
//       - Start child dispatch_order workflow
//       - Await child completion
//   4b. Payment failed / timeout:
//       - Mark order cancelled (durableCall to "orders" service)
//       - Release reserved inventory (durableCall to "inventory" service)
//   5. Set query state for HTTP handlers to retrieve order ID
//
// Compare with TypeScript checkoutWorkflow in main.ts which uses
// DBOS.registerWorkflow, DBOS.recv, DBOS.setEvent, DBOS.startWorkflow.
//
// Input JSON:
//   {"user_id":"...","items":["widget-a","widget-b"]}
//
// Output JSON (success):
//   {"status":"shipped|paid|cancelled","order_id":"...","reservation_id":"..."}
//
// Output JSON (error):
//   {"error":"..."}
// ══════════════════════════════════════════════════════════════════════════════

export function checkout_workflow(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h: HostCalls = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let userID: string = extractStringField(argsJson, "user_id");
  let itemsJson: string = extractRawArray(argsJson, "items");

  if (itemsJson.length <= 2) {
    return writeError(outPtr, maxOutLen, "cart is empty");
  }

  // ---- Step 1: Reserve inventory ------------------------------------------

  let reserveReq: string = '{"user_id":"' + userID + '","items":' + itemsJson + '}';
  let reserveResult: DurableCallOutcome = h.durableCall("inventory", "Reserve", reserveReq);
  if (reserveResult.isError) {
    return writeError(outPtr, maxOutLen, "inventory reserve failed: " + safeStr(reserveResult.error));
  }
  let reservationID: string = extractStringField(reserveResult.response, "reservation_id");

  // ---- Step 2: Create order -----------------------------------------------

  let createReq: string = '{"user_id":"' + userID + '","reservation_id":"' + reservationID + '"}';
  let createResult: DurableCallOutcome = h.durableCall("orders", "Create", createReq);
  if (createResult.isError) {
    // Compensate: release reserved inventory.
    h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    return writeError(outPtr, maxOutLen, "order creation failed: " + safeStr(createResult.error));
  }
  let orderID: string = extractStringField(createResult.response, "order_id");

  // ---- Step 3: Set query state for HTTP handlers --------------------------

  // Equivalent of DBOS.setEvent(PAYMENT_ID_EVENT, workflowID) in TypeScript.
  // The HTTP endpoint polls this to get the workflow ID for signal delivery.
  h.setQueryState("workflow_id", h.currentWorkflowId());
  h.setQueryState("order_id", orderID);

  // ---- Step 4: Wait for payment signal ------------------------------------
  //
  // Equivalent of DBOS.recv<string>(PAYMENT_TOPIC, 120) in TypeScript.
  //
  // NOTE: awaitSignals may not properly detect host suspension (bit 62).
  // If no signal arrives before timeout, timedOut is set to true.
  // See ISSUES.md for details.

  let signalResult: AwaitSignalsOutcome = h.awaitSignals('["payment_received"]', 120000);

  // Check for host suspension (bit 62) — workaround for potential SDK issue.
  // If awaitSignals returns empty result (no signal name, no payload, not timed out,
  // no error), the host may have signaled suspension but the SDK didn't detect it.
  if (
    signalResult.signalName.length === 0 &&
    signalResult.payload.length === 0 &&
    !signalResult.timedOut &&
    !signalResult.isError
  ) {
    // Host likely needs to suspend — return sentinel.
    // This is a provisional check; see ISSUES.md for details.
    return SUSPEND_SENTINEL;
  }

  if (signalResult.timedOut) {
    // Payment timed out — compensate.
    h.durableCall("orders", "Cancel", '{"order_id":"' + orderID + '"}');
    h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    h.setQueryState("payment_status", "timeout");
    return writeResult(outPtr, maxOutLen,
      '{"status":"cancelled","order_id":"' + orderID + '","reason":"payment timeout"}');
  }

  if (signalResult.isError) {
    // Signal error — compensate.
    h.durableCall("orders", "Cancel", '{"order_id":"' + orderID + '"}');
    h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    return writeError(outPtr, maxOutLen, "payment signal error: " + safeStr(signalResult.error));
  }

  // ---- Step 4a: Payment received — process result -------------------------

  let paymentStatus: string = extractStringField(signalResult.payload, "status");

  if (paymentStatus == "paid") {
    // Mark order as paid.
    let markPaidReq: string = '{"order_id":"' + orderID + '"}';
    let paidResult: DurableCallOutcome = h.durableCall("orders", "MarkPaid", markPaidReq);
    if (paidResult.isError) {
      // Compensate: cancel order and release inventory.
      h.durableCall("orders", "Cancel", '{"order_id":"' + orderID + '"}');
      h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
      return writeError(outPtr, maxOutLen,
        "marking order paid failed: " + safeStr(paidResult.error));
    }

    // ---- Step 4a.1: Start child dispatch workflow -------------------------
    //
    // Equivalent of DBOS.startWorkflow(dispatchOrder)(orderID) in TypeScript.

    let dispatchInput: string = '{"order_id":"' + orderID + '"}';
    let dispatchResult: DurableResult<string> = h.childWorkflow("dispatch_order", dispatchInput);
    if (dispatchResult.isError) {
      // Child start failed — order is already paid, so log and continue.
      h.log("dispatch_order start failed (order already paid): " + safeStr(dispatchResult.error));
    } else {
      // Await child completion.
      let awaitResult: DurableResult<string> = h.awaitChild(dispatchResult.value);

      // Check if the workflow was suspended while awaiting child.
      if (isWorkflowSuspended()) {
        return SUSPEND_SENTINEL;
      }

      if (awaitResult.isError && awaitResult.value.length > 0) {
        // Child had an error — log but order is already paid.
        h.log("dispatch_order completed with issues: " + safeStr(awaitResult.error));
      }
    }

    // Set final query state.
    h.setQueryState("payment_status", "paid");

    let resultJson: string =
      '{"status":"shipped","order_id":"' + orderID + '","reservation_id":"' + reservationID + '"}';
    return writeResult(outPtr, maxOutLen, resultJson);

  } else {
    // ---- Step 4b: Payment failed — compensate -----------------------------
    //
    // Equivalent of TypeScript callback branch:
    //   await errorOrder(orderID);
    //   await undoSubtractInventory();

    h.durableCall("orders", "Cancel", '{"order_id":"' + orderID + '"}');
    h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');

    h.setQueryState("payment_status", "failed");

    return writeResult(outPtr, maxOutLen,
      '{"status":"cancelled","order_id":"' + orderID + '","reason":"payment ' + paymentStatus + '"}');
  }
}

// ══════════════════════════════════════════════════════════════════════════════
// Workflow: dispatch_order
//
// Child workflow that simulates order dispatch with progress updates.
// Called by checkout_workflow after payment is received.
//
// Compares with TypeScript dispatchOrder in shop.ts which uses a loop of
// DBOS.sleep(1000) + updateOrderProgress() with 10 iterations.
//
// This workflow demonstrates:
//   - durableSleep (workflow suspension and replay)
//   - DurableCall for progress updates
//   - Child workflow ABI export pattern
//
// Input JSON:
//   {"order_id":"..."}
//
// Output JSON (success):
//   {"status":"dispatched","order_id":"..."}
//
// NOTE: durableSleep sets the suspension flag. After checking, if the workflow
// was suspended, return SUSPEND_SENTINEL so the host can resume later.
// ══════════════════════════════════════════════════════════════════════════════

export function dispatch_order(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h: HostCalls = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let orderID: string = extractStringField(argsJson, "order_id");

  if (orderID.length === 0) {
    return writeError(outPtr, maxOutLen, "dispatch_order: missing order_id");
  }

  // ---- Step 1-10: Dispatch loop with sleep and progress updates -----------
  //
  // On fresh execution: durableSleep(1000) returns true, setting the
  // suspension flag. The workflow returns SUSPEND_SENTINEL.
  // On replay: durableSleep(1000) returns false immediately, and control
  // flows to the durableCall. The host replays the durableCall result.
  //
  // The loop body executes as many steps as it can before the next suspend,
  // exactly matching the original DBOS durability semantics.

  for (let i: i32 = 0; i < 10; i++) {
    // Sleep 1 second between progress steps.
    h.durableSleep(1000);
    if (isWorkflowSuspended()) {
      return SUSPEND_SENTINEL;
    }

    // Update progress via durable call.
    let progressReq: string =
      '{"order_id":"' + orderID + '","step":' + (i + 1).toString() + ',"total":10}';
    let progressResult: DurableCallOutcome = h.durableCall("orders", "UpdateProgress", progressReq);
    if (progressResult.isError) {
      // Best-effort: log the error but continue dispatch.
      h.log("UpdateProgress warning (" + (i + 1).toString() + "/10): " + safeStr(progressResult.error));
    }
  }

  // ---- Step 11: Mark order as dispatched ----------------------------------

  let dispatchReq: string = '{"order_id":"' + orderID + '"}';
  let dispatchResult: DurableCallOutcome = h.durableCall("orders", "MarkDispatched", dispatchReq);
  if (dispatchResult.isError) {
    // Log but still return success — the dispatch loop completed.
    h.log("MarkDispatched warning: " + safeStr(dispatchResult.error));
  }

  return writeResult(outPtr, maxOutLen,
    '{"status":"dispatched","order_id":"' + orderID + '"}');
}

// ══════════════════════════════════════════════════════════════════════════════
// Workflow: cancel_order
//
// Cancellation-aware checkout workflow.
// Checks for cancellation before taking any irreversible actions.
//
// This demonstrates the pollCancellation() API and cancellation-aware
// workflow patterns in AssemblyScript.
//
// Input JSON:
//   {"user_id":"...","items":[...]}
//
// Output JSON (success):
//   {"status":"cancelled|reserved","reason":"...","reservation_id":"..."}
//
// Output JSON (error):
//   {"error":"..."}
// ══════════════════════════════════════════════════════════════════════════════

export function cancel_order(
  argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32
): i64 {
  let h: HostCalls = new HostCalls();

  // ---- Read and parse input ----
  let argsJson: string = argsLen > 0 ? readString(argsPtr, argsLen) : "";
  let userID: string = extractStringField(argsJson, "user_id");
  let itemsJson: string = extractRawArray(argsJson, "items");

  // ---- Check cancellation before processing ----
  let cancelledStatus: CancellationStatus = h.pollCancellation();
  if (cancelledStatus.cancelled) {
    return writeResult(outPtr, maxOutLen,
      '{"status":"cancelled","reason":"' + cancelledStatus.reason + '"}');
  }

  if (itemsJson.length <= 2) {
    return writeError(outPtr, maxOutLen, "cart is empty");
  }

  // ---- Reserve inventory (reversible) ----
  let reserveReq: string = '{"user_id":"' + userID + '","items":' + itemsJson + '}';
  let reserveResult: DurableCallOutcome = h.durableCall("inventory", "Reserve", reserveReq);
  if (reserveResult.isError) {
    return writeError(outPtr, maxOutLen,
      "inventory reserve failed: " + safeStr(reserveResult.error));
  }

  let reservationID: string = extractStringField(reserveResult.response, "reservation_id");

  // ---- Check cancellation again after reserve ----
  cancelledStatus = h.pollCancellation();
  if (cancelledStatus.cancelled) {
    // Compensate: release inventory.
    h.durableCall("inventory", "Release", '{"reservation_id":"' + reservationID + '"}');
    return writeResult(outPtr, maxOutLen,
      '{"status":"cancelled","reason":"' + cancelledStatus.reason + '","reservation_id":"' + reservationID + '"}');
  }

  return writeResult(outPtr, maxOutLen,
    '{"status":"reserved","reservation_id":"' + reservationID + '"}');
}
