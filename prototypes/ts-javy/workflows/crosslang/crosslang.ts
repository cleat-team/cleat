import { workflow } from "../../packages/cleat-ts/index";

// Q4: the TypeScript twin of testdata/crosslang (Go) and examples/rust-workflow (Rust): the same `place_order`, the same host
// calls in the same order, including the four DurableLogs. Input is the flat snake_case shape the Rust twin takes; the Go twin's
// is wrapped under its parameter name ({"input": {...}}), which is a property of the Go SDK, not of the workflow.
// Requests go through the host's canonical JSON (ABI 6.3, "SHOULD"); whether that changes anything replay can see is measured, not assumed.

interface CartItem {
  sku: string;
  quantity: number;
}

export const placeOrder = workflow<{ user_id: string; cart: CartItem[] }, { tracking_id: string }>(async (ctx, input) => {
  if (!input.cart || input.cart.length === 0) throw new Error("cart is empty");
  ctx.log(`Starting order for user ${input.user_id}`);
  const req = (v: unknown) => ctx.canonical(JSON.stringify(v));

  let reservation: { reservation_id: string; total_cents: number };
  try {
    reservation = (await ctx.callRaw("inventory", "Reserve", req({ user_id: input.user_id, cart: input.cart }))) as typeof reservation;
  } catch (e) {
    throw new Error(`inventory reserve failed: ${(e as Error).message}`);
  }
  ctx.log(`Reserved inventory: ${reservation.reservation_id}`);

  let charge: { charge_id: string; amount: number };
  try {
    charge = (await ctx.callRaw("payments", "Charge", req({ user_id: input.user_id, amount_cents: reservation.total_cents }))) as typeof charge;
  } catch (e) {
    await ctx.callRaw("inventory", "Release", req({ reservation_id: reservation.reservation_id }));
    throw new Error(`payment failed: ${(e as Error).message}`);
  }
  ctx.log(`Payment processed: ${charge.charge_id}`);

  let tracking: { tracking_id: string };
  try {
    tracking = (await ctx.callRaw("shipping", "CreateShipment", req({ reservation_id: reservation.reservation_id, charge_id: charge.charge_id }))) as typeof tracking;
  } catch (e) {
    await ctx.callRaw("payments", "Refund", req({ charge_id: charge.charge_id }));
    await ctx.callRaw("inventory", "Release", req({ reservation_id: reservation.reservation_id }));
    throw new Error(`fulfillment failed: ${(e as Error).message}`);
  }
  ctx.log(`Order complete: ${tracking.tracking_id}`);

  await ctx.callRaw("notifications", "SendEmail", req({ user_id: input.user_id, tracking_id: tracking.tracking_id }));
  return { tracking_id: tracking.tracking_id };
});
