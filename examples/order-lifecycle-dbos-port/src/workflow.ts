// Order lifecycle -- a saga over a payment provider, with compensation.
//
// The DBOS counterpart to cleat's examples/order-lifecycle/order.go, at the
// same scope: a card charge, an inventory reservation, a shipment step, and
// compensation that unwinds completed steps when a later one fails. Written
// against @dbos-inc/dbos-sdk 5.1.10, the current version as of 2026-09-28 --
// see the pair's README for why the version and the date both matter.
//
// DBOS has no saga primitive (cleat.NewSaga's undo-on-failure has no
// equivalent in the SDK's public API, confirmed against docs.dbos.dev's
// workflow tutorial, which describes retryable steps but not compensation).
// So the saga here is hand-written: each step's compensation is a sibling
// function, called from a catch block, in reverse order of which steps
// completed. That is part of the 17 "tenancy"-role lines the measurement doc
// counts on the DBOS side having no platform equivalent for.
import { DBOS } from '@dbos-inc/dbos-sdk';

export interface OrderItem {
  sku: string;
  quantity: number;
  priceCents: number;
}

export interface OrderInput {
  orderId: string;
  customerId: string;
  email: string;
  items: OrderItem[];
  // simulatePaymentFailure makes the PSP placeholder decline, so the
  // COMPENSATING path (nothing to compensate yet -- the charge is the first
  // step) is reachable.
  simulatePaymentFailure: boolean;
  // simulateFulfilmentFailure fails the step AFTER the charge, so the refund
  // is a real unwind of a real charge rather than an unwind of nothing --
  // the case the saga exists for.
  simulateFulfilmentFailure: boolean;
  // simulateCompensationFailure makes the inventory-release step of THAT
  // unwind fail. "The compensation ran" and "the compensation worked" are
  // different claims, and only simulateFulfilmentFailure alone cannot reach
  // the second: it needs something completed to release, so this flag only
  // has an effect combined with simulateFulfilmentFailure.
  simulateCompensationFailure: boolean;
}

export type OrderStatus = 'shipped' | 'declined' | 'compensated' | 'compensation_failed';

export interface OrderResult {
  orderId: string;
  totalCents: number;
  status: OrderStatus;
  stepsCompleted: number;
}

// placeholderRoundTripMs stands where a network round trip to a PSP,
// inventory system or 3PL goes -- DBOS.sleep rather than a bare setTimeout,
// for the reason that matters: DBOS.sleep is checkpointed, so a process that
// crashes mid-sleep resumes past it on recovery rather than sleeping again.
const placeholderRoundTripMs = 120;

function orderTotalCents(items: OrderItem[]): number {
  return items.reduce((sum, i) => sum + i.priceCents * i.quantity, 0);
}

// ---- Steps ----

async function chargeCard(orderId: string, amount: number): Promise<void> {
  await DBOS.sleep(placeholderRoundTripMs);
  DBOS.logger.info(`charged ${amount} cents for order ${orderId}`);
}

async function refundCard(orderId: string, amount: number): Promise<void> {
  await DBOS.sleep(placeholderRoundTripMs);
  DBOS.logger.info(`refunded ${amount} cents for order ${orderId}`);
}

async function reserveInventory(orderId: string, items: OrderItem[], fail: boolean): Promise<void> {
  await DBOS.sleep(placeholderRoundTripMs);
  if (fail) {
    throw new Error(`inventory reservation failed for order ${orderId}`);
  }
  DBOS.logger.info(`reserved ${items.length} line item(s) for order ${orderId}`);
}

// Returns false on failure rather than throwing: a compensation that fails
// must be reported, not treated as an ordinary step error -- there is
// nothing further to compensate for a compensation.
async function releaseInventory(orderId: string, items: OrderItem[], fail: boolean): Promise<boolean> {
  await DBOS.sleep(placeholderRoundTripMs);
  if (fail) {
    DBOS.logger.error(`inventory release FAILED for order ${orderId} -- order is charged and unshipped`);
    return false;
  }
  DBOS.logger.info(`released ${items.length} line item(s) for order ${orderId}`);
  return true;
}

async function shipOrder(orderId: string, fail: boolean): Promise<void> {
  await DBOS.sleep(placeholderRoundTripMs);
  if (fail) {
    throw new Error(`shipment failed for order ${orderId}`);
  }
  DBOS.logger.info(`shipped order ${orderId}`);
}

// ---- The workflow ----
//
// One entry point, matching cleat's single PlaceOrder -- the three
// simulate* flags select which branch runs, the same shape order.go uses.

async function orderLifecycle(input: OrderInput): Promise<OrderResult> {
  const amount = orderTotalCents(input.items);
  let stepsCompleted = 0;

  await DBOS.runStep(() => chargeCard(input.orderId, amount), { name: 'chargeCard' });
  stepsCompleted++;

  // The PSP's confirmation arrives over DBOS.recv, delivered by the webhook
  // handler in server.ts -- mirrors the cleat side's await_webhook against a
  // bundled plugin, and is a real durable wait, not a synchronous return.
  const confirmation = input.simulatePaymentFailure
    ? null
    : await DBOS.recv<string>('payment-confirmed', { timeoutSeconds: 30 });

  if (confirmation === null) {
    // Compensating the FIRST step -- the cheapest case, and the one every
    // saga demo reaches for. It is deliberately not the only one exercised.
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return { orderId: input.orderId, totalCents: amount, status: 'declined', stepsCompleted };
  }

  try {
    await DBOS.runStep(
      () => reserveInventory(input.orderId, input.items, input.simulateFulfilmentFailure),
      { name: 'reserveInventory' },
    );
    stepsCompleted++;
  } catch {
    // The case the saga exists for: the charge already completed, so this
    // unwind is real. releaseInventory never ran (reserveInventory threw
    // before completing), so only the charge needs refunding.
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return { orderId: input.orderId, totalCents: amount, status: 'compensated', stepsCompleted };
  }

  await DBOS.runStep(() => shipOrder(input.orderId, false), { name: 'shipOrder' });
  stepsCompleted++;
  return { orderId: input.orderId, totalCents: amount, status: 'shipped', stepsCompleted };
}

// A second entry point for the one case orderLifecycle's happy-reservation
// path cannot reach: a reservation that SUCCEEDS, so there is something to
// release, and a later failure whose unwind calls releaseInventory with
// simulateCompensationFailure threaded through. Kept separate rather than
// adding a fourth branch to orderLifecycle, because the two workflows differ
// in which step fails (reserveInventory vs. the step after it), not merely
// in a flag value -- collapsing them would need a state machine bigger than
// either the cleat side's saga declaration or this file already is.
async function orderLifecycleFulfilmentFailsAfterReservation(input: OrderInput): Promise<OrderResult> {
  const amount = orderTotalCents(input.items);
  await DBOS.runStep(() => chargeCard(input.orderId, amount), { name: 'chargeCard' });
  const confirmation = await DBOS.recv<string>('payment-confirmed', { timeoutSeconds: 30 });
  if (confirmation === null) {
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return { orderId: input.orderId, totalCents: amount, status: 'declined', stepsCompleted: 1 };
  }
  await DBOS.runStep(() => reserveInventory(input.orderId, input.items, false), { name: 'reserveInventory' });

  // shipOrder is where the simulated failure lands -- reservation is real
  // and completed, so its release is a genuine unwind of held stock.
  try {
    await DBOS.runStep(() => shipOrder(input.orderId, input.simulateFulfilmentFailure), { name: 'shipOrder' });
    return { orderId: input.orderId, totalCents: amount, status: 'shipped', stepsCompleted: 3 };
  } catch {
    const released = await DBOS.runStep(
      () => releaseInventory(input.orderId, input.items, input.simulateCompensationFailure),
      { name: 'releaseInventory' },
    );
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return {
      orderId: input.orderId,
      totalCents: amount,
      status: released ? 'compensated' : 'compensation_failed',
      stepsCompleted: 2,
    };
  }
}

export const OrderLifecycle = DBOS.registerWorkflow(orderLifecycle, { name: 'orderLifecycle' });
export const OrderLifecycleFulfilmentFailsAfterReservation = DBOS.registerWorkflow(
  orderLifecycleFulfilmentFailsAfterReservation,
  { name: 'orderLifecycleFulfilmentFailsAfterReservation' },
);
