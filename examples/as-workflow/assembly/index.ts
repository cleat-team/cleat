// AssemblyScript workflow example for cleat durable execution.
// Uses the @cleat/sdk module and @durableEntry decorator.
//
// Compiled to WASM with: npx asc assembly/index.ts --runtime stub --optimize --initialMemory 170 -o dist/workflow.wasm

import { HostCalls, durableEntry } from "@cleat/sdk";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

@json
class CartItem {
  sku: string = "";
  quantity: i32 = 0;
}

@json
class Reservation {
  reservationID: string = "";
  totalCents: i64 = 0;
}

@json
class Charge {
  chargeID: string = "";
  amountCents: i64 = 0;
}

@json
class PlaceOrderInput {
  userID: string = "";
  items: CartItem[] = [];
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

@durableEntry
function place_order(h: HostCalls, input: PlaceOrderInput): string {
  if (input.items.length == 0) {
    return `{"error":"cart is empty"}`;
  }

  // --- Step 1: Reserve inventory -------------------------------------------

  let reserveReq = `{"user_id":"${input.userID}","items":${serializeItems(input.items)}}`;
  let reserveResult = h.durableCall("inventory", "Reserve", reserveReq);
  if (!reserveResult.ok) {
    return `{"error":"inventory reserve failed: ${reserveResult.error}"}`;
  }
  let reservation = JSON.parse<Reservation>(reserveResult.value);

  // --- Step 2: Charge payment ----------------------------------------------

  let chargeReq = `{"user_id":"${input.userID}","amount_cents":${reservation.totalCents}}`;
  let chargeResult = h.durableCall("payments", "Charge", chargeReq);
  if (!chargeResult.ok) {
    // Compensate: release the reserved inventory.
    h.durableCall("inventory", "Release", `{"reservation_id":"${reservation.reservationID}"}`);
    return `{"error":"payment failed: ${chargeResult.error}"}`;
  }
  let charge = JSON.parse<Charge>(chargeResult.value);

  // --- Step 3: Create shipment --------------------------------------------

  let shipReq = `{"reservation_id":"${reservation.reservationID}","charge_id":"${charge.chargeID}"}`;
  let shipResult = h.durableCall("shipping", "CreateShipment", shipReq);
  if (!shipResult.ok) {
    // Compensate: refund the payment and release inventory.
    h.durableCall("payments", "Refund", `{"charge_id":"${charge.chargeID}"}`);
    h.durableCall("inventory", "Release", `{"reservation_id":"${reservation.reservationID}"}`);
    return `{"error":"shipping failed: ${shipResult.error}"}`;
  }

  // --- Step 4: Notify customer (best-effort) -------------------------------

  h.durableCall("notifications", "SendEmail", `{"user_id":"${input.userID}","message":"Order shipped"}`);

  return `{"status":"shipped","reservation_id":"${reservation.reservationID}"}`;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function serializeItems(items: CartItem[]): string {
  let parts: string[] = [];
  for (let i = 0; i < items.length; i++) {
    parts.push(`{"sku":"${items[i].sku}","quantity":${items[i].quantity}}`);
  }
  return "[" + parts.join(",") + "]";
}

// ---------------------------------------------------------------------------
// Workflow: cancel_order
//
// A cancellation-aware entry point that checks cancellation before proceeding.
// ---------------------------------------------------------------------------

@durableEntry
function cancel_order(h: HostCalls, input: PlaceOrderInput): string {
  let cancelled = h.pollCancellation();
  if (cancelled) {
    return `{"status":"cancelled","reason":"cancelled before processing"}`;
  }

  if (input.items.length == 0) {
    return `{"error":"cart is empty"}`;
  }

  let reserveReq = `{"user_id":"${input.userID}","items":${serializeItems(input.items)}}`;
  let reserveResult = h.durableCall("inventory", "Reserve", reserveReq);
  if (!reserveResult.ok) {
    return `{"error":"inventory reserve failed: ${reserveResult.error}"}`;
  }

  return `{"status":"reserved"}`;
}
