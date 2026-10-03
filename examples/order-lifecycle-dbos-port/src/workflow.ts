// Order lifecycle -- a saga over a payment provider, with compensation.
//
// The DBOS counterpart to cleat's examples/order-lifecycle/order.go, at the
// same scope: a card charge, an inventory reservation, a shipment step,
// compensation that unwinds completed steps when a later one fails, a
// human-approval gate above a threshold, and query state a poller can read.
// The last two were added by cleat#2997: without them the pair's "same scope"
// claim was false and the headline compared a 5-step application against a
// 3-step one. Written against @dbos-inc/dbos-sdk 5.2.11, the version
// package.json pins -- see the pair's README for why the version and the date
// both matter.
//
// DBOS has no saga primitive (cleat.NewSaga's undo-on-failure has no
// equivalent in the SDK's public API, confirmed against docs.dbos.dev's
// workflow tutorial, which describes retryable steps but not compensation).
// So the saga here is hand-written: each step's compensation is a sibling
// function, called from a catch block, in reverse order of which steps
// completed. That is part of the 17 "tenancy"-role lines the measurement doc
// counts on the DBOS side having no platform equivalent for.
import { DBOS } from '@dbos-inc/dbos-sdk';

// approvalThresholdCents is the order value above which a human has to decide
// before the card is charged. It mirrors cleat's ApprovalThresholdCents
// (order.go), deliberately at the same 50_000, so the two sides gate on the
// same orders.
export const approvalThresholdCents = 50_000;

// DECISION_TOPIC is the one topic the approval decision arrives on, and it is
// one topic rather than two because DBOS has no multi-signal wait: `recv`
// takes a SINGLE topic per call, and `waitFirst`/`waitAll` operate on workflow
// handles rather than on messages. cleat's AwaitSignals takes a LIST of signal
// names; the equivalent here is one topic whose payload names the decision,
// which is the idiom DBOS's own human-in-the-loop example uses. Same scope,
// different shape -- stated in the pair's README rather than left for a reader
// to find.
export const DECISION_TOPIC = 'order-decision';

// defaultApprovalWindowSeconds is 24h, matching cleat's ApprovalTimeout. It is
// a DEFAULT for an input field rather than a bare constant, and that is a real
// difference from cleat's side worth naming: cleat's test fast-forwards 24h of
// SIMULATED time with AdvanceTime, and DBOS has no simulated clock, so a DBOS
// test of the timeout path can only wait real seconds. Carrying it in the
// durable input is also the determinism-safe place for it -- a process.env
// read inside a workflow is not checkpointed and could differ across a
// restart, where the input cannot.
const defaultApprovalWindowSeconds = 24 * 60 * 60;

// ApprovalDecision is the payload the decision topic carries.
export interface ApprovalDecision {
  approved: boolean;
  reason?: string;
}

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

  // approvalWindowSeconds overrides how long an above-threshold order waits
  // for a decision. Unset takes defaultApprovalWindowSeconds (24h). It exists
  // as an input rather than a constant so the timeout path is reachable in a
  // test without waiting a day of real time -- see
  // defaultApprovalWindowSeconds above for why that is a real difference from
  // cleat's side rather than a convenience.
  approvalWindowSeconds?: number;
}

export type OrderStatus =
  | 'shipped'
  | 'declined'
  | 'compensated'
  | 'compensation_failed'
  // rejected is the approval gate's terminal outcome: the order never reached
  // a spending step, so it has no compensation trail. cleat carries the same
  // status (order.go's rejection branch).
  | 'rejected';

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

// ---- Query state and the approval gate ----

// finish publishes the terminal status and returns the result, so that every
// exit path leaves the published state agreeing with what the workflow
// returned. It exists because DBOS has no equivalent of cleat's saga progress
// publishing (order.go notes that cleat#2627 MOVED that bookkeeping into
// cleat/runtime_workflow.go, shrinking the app) -- here the app writes each
// transition itself, and this is the last one.
async function finish(result: OrderResult): Promise<OrderResult> {
  await DBOS.setEvent('status', result.status);
  return result;
}

// approvalGate publishes the order's opening query state and, above the
// threshold, waits for a human decision. It returns null when the order may
// proceed, or the terminal result to return when it may not.
//
// THE GATE SITS BEFORE THE FIRST SPENDING STEP, and that placement is the
// whole point: a rejected order must leave no compensation trail, because it
// never spent anything. cleat's order.go puts it in the same place for the
// same reason, and the test for it ("a rejected order spends nothing") is
// ported below -- an implementation that gated AFTER the charge would show up
// there as a non-empty compensation.
//
// setEvent and recv are workflow-context calls, not steps: they must not be
// wrapped in runStep, and this helper is only ever called from a workflow.
async function approvalGate(input: OrderInput, amount: number): Promise<OrderResult | null> {
  // Published before anything can fail, so a poller has something to show
  // from the first moment rather than reading nothing at all.
  await DBOS.setEvent('order_id', input.orderId);
  await DBOS.setEvent('total_cents', amount);
  await DBOS.setEvent('status', 'validated');

  if (amount <= approvalThresholdCents) {
    return null;
  }

  await DBOS.setEvent('status', 'awaiting_approval');
  DBOS.logger.info(`order ${input.orderId} awaiting approval (${amount} cents)`);

  const windowSeconds = input.approvalWindowSeconds ?? defaultApprovalWindowSeconds;
  const decision = await DBOS.recv<ApprovalDecision>(DECISION_TOPIC, { timeoutSeconds: windowSeconds });

  // A timeout and a refusal are different events, and the reason distinguishes
  // them: an order nobody decided on is not the same as one somebody turned
  // down, and a poller reading the reason can tell them apart.
  if (decision === null) {
    await DBOS.setEvent('status', 'rejected');
    await DBOS.setEvent('rejection_reason', `no approval within ${windowSeconds}s`);
    return { orderId: input.orderId, totalCents: amount, status: 'rejected', stepsCompleted: 0 };
  }
  if (!decision.approved) {
    await DBOS.setEvent('status', 'rejected');
    await DBOS.setEvent('rejection_reason', decision.reason ?? 'rejected without a reason');
    return { orderId: input.orderId, totalCents: amount, status: 'rejected', stepsCompleted: 0 };
  }

  // The decision is in and the order is no longer waiting on one, so the
  // published status has to move off "awaiting_approval". cleat's order.go
  // carries the same write and records why: without it a poller describes an
  // order that is being charged as still awaiting a decision. cleat#2627.
  await DBOS.setEvent('status', 'approved');
  return null;
}

// ---- The workflow ----
//
// One entry point, matching cleat's single PlaceOrder -- the three
// simulate* flags select which branch runs, the same shape order.go uses.

async function orderLifecycle(input: OrderInput): Promise<OrderResult> {
  const amount = orderTotalCents(input.items);
  let stepsCompleted = 0;

  const rejected = await approvalGate(input, amount);
  if (rejected !== null) {
    return rejected;
  }

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
    return finish({ orderId: input.orderId, totalCents: amount, status: 'declined', stepsCompleted });
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
    return finish({ orderId: input.orderId, totalCents: amount, status: 'compensated', stepsCompleted });
  }

  await DBOS.runStep(() => shipOrder(input.orderId, false), { name: 'shipOrder' });
  stepsCompleted++;
  return finish({ orderId: input.orderId, totalCents: amount, status: 'shipped', stepsCompleted });
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

  const rejected = await approvalGate(input, amount);
  if (rejected !== null) {
    return rejected;
  }

  await DBOS.runStep(() => chargeCard(input.orderId, amount), { name: 'chargeCard' });
  const confirmation = await DBOS.recv<string>('payment-confirmed', { timeoutSeconds: 30 });
  if (confirmation === null) {
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return finish({ orderId: input.orderId, totalCents: amount, status: 'declined', stepsCompleted: 1 });
  }
  await DBOS.runStep(() => reserveInventory(input.orderId, input.items, false), { name: 'reserveInventory' });

  // shipOrder is where the simulated failure lands -- reservation is real
  // and completed, so its release is a genuine unwind of held stock.
  try {
    await DBOS.runStep(() => shipOrder(input.orderId, input.simulateFulfilmentFailure), { name: 'shipOrder' });
    return finish({ orderId: input.orderId, totalCents: amount, status: 'shipped', stepsCompleted: 3 });
  } catch {
    const released = await DBOS.runStep(
      () => releaseInventory(input.orderId, input.items, input.simulateCompensationFailure),
      { name: 'releaseInventory' },
    );
    await DBOS.runStep(() => refundCard(input.orderId, amount), { name: 'refundCard' });
    return finish({
      orderId: input.orderId,
      totalCents: amount,
      status: released ? 'compensated' : 'compensation_failed',
      stepsCompleted: 2,
    });
  }
}

export const OrderLifecycle = DBOS.registerWorkflow(orderLifecycle, { name: 'orderLifecycle' });
export const OrderLifecycleFulfilmentFailsAfterReservation = DBOS.registerWorkflow(
  orderLifecycleFulfilmentFailsAfterReservation,
  { name: 'orderLifecycleFulfilmentFailsAfterReservation' },
);
