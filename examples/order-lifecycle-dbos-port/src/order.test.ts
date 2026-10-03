// Executed assertions against a real DBOS runtime and a real Postgres -- not
// a description of expected behaviour. Run with `npm run build && npm test`
// against DBOS_SYSTEM_DATABASE_URL. No test framework: the scenarios each
// assert the OUTCOME a workflow PUBLISHED, mirroring the shape
// examples/order-lifecycle/order_test.go asserts (status, not internals).
//
// THE CLAIM THIS PAIR CARRIES (cleat#2597 criterion 3) -- this is the
// CONTROL pair, so the claim is about the honesty of the comparison, not
// about which side wins:
//
//   "A properly scoped, idiomatic, EXECUTED DBOS port of order-lifecycle can
//    be measured against cleat's on equal terms (same cloc invocation, same
//    scenario, both run in CI) -- and on this scenario, today, that
//    comparison does not favour cleat."
//
// The claim said "equally-scoped" until cleat#2997, and it was not true: the
// cleat side carried a human-approval gate and published query state, and
// this port carried neither. Both were added, and the differences that
// REMAIN are named in the pair's README rather than left for a reader to
// find -- so this constant now says what is true, which is that the two
// sides are equal on the dimensions that were missing and not step-for-step
// identical.
//
// If a future cleat feature (item (a) in the 0.4.0 levers list: a blocking
// durable wait for webhooks/signals, replacing the found:false polling loop)
// changes which side is smaller, this pair's job is unaffected: it does not
// assert a winner, it asserts that both sides are real and comparable. See
// the pair's README for the cloc numbers.
import { DBOS } from '@dbos-inc/dbos-sdk';
import {
  ApprovalDecision,
  approvalThresholdCents,
  DECISION_TOPIC,
  OrderLifecycle,
  OrderLifecycleFulfilmentFailsAfterReservation,
  OrderInput,
} from './workflow';

const CLAIM =
  'order-lifecycle-dbos-port: a genuinely executed DBOS counterpart exists, ' +
  "measurable against cleat's by the same line counter, carrying the same " +
  'approval gate and published query state (cleat#2997). The remaining ' +
  'structural differences -- no notification step, and the payment ' +
  'confirmation is a recv rather than a saga step -- are stated in the ' +
  "README's table, not hidden. This pair asserts comparability, not a " +
  'winner (cleat#2597).';

function baseInput(orderId: string): OrderInput {
  return {
    orderId,
    customerId: 'cust-1',
    email: 'a@example.com',
    items: [{ sku: 'sku-1', quantity: 2, priceCents: 500 }],
    simulatePaymentFailure: false,
    simulateFulfilmentFailure: false,
    simulateCompensationFailure: false,
  };
}

// aboveThreshold is the same order shape every gate test uses: one line item
// costing one cent more than the threshold, so the gate is exercised and the
// margin is unambiguous.
function aboveThreshold(orderId: string): OrderInput {
  return {
    ...baseInput(orderId),
    items: [{ sku: 'server', quantity: 1, priceCents: approvalThresholdCents + 1 }],
  };
}

let failures = 0;

function assertEqual<T>(got: T, want: T, why: string) {
  if (got !== want) {
    failures++;
    console.error(`FAIL: ${why} -- got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  } else {
    console.log(`ok: ${why}`);
  }
}

// stateOf reads one published query-state key. A zero timeout is deliberate:
// getEvent otherwise WAITS for the event to appear, which would hang the
// suite on a key this order never sets.
async function stateOf(workflowID: string, key: string): Promise<unknown> {
  return await DBOS.getEvent<unknown>(workflowID, key, { timeoutSeconds: 0 });
}

async function testHappyPath() {
  const input = baseInput('order-happy');
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(result.status, 'shipped', 'a confirmed payment and a clean reservation ships');
  assertEqual(result.stepsCompleted, 3, 'all three steps ran');
  // The workflow publishes its outcome, so a poller reads the same answer the
  // caller got without reading the event history (cleat#2997).
  assertEqual(await stateOf(handle.workflowID, 'status'), 'shipped', 'the published status ends at the outcome');
  assertEqual(await stateOf(handle.workflowID, 'order_id'), 'order-happy', 'the order id is published');
}

async function testDeclinedNoConfirmation() {
  const input = { ...baseInput('order-declined'), simulatePaymentFailure: true };
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);
  // No DBOS.send -- simulatePaymentFailure short-circuits before the recv,
  // so this proves the DECLINE path, not the timeout path (see below).
  const result = await handle.getResult();
  assertEqual(result.status, 'declined', 'a simulated decline never reaches reservation');
  assertEqual(result.stepsCompleted, 1, 'only the charge (later refunded) ran');
}

async function testReservationFailsAfterCharge() {
  const input = { ...baseInput('order-compensated'), simulateFulfilmentFailure: true };
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(result.status, 'compensated', 'a charge that completed and then failed to reserve unwinds');
  assertEqual(result.stepsCompleted, 1, 'only the charge counts as completed; reservation threw');
}

async function testCompensationItselfFails() {
  const input = {
    ...baseInput('order-compensation-failed'),
    simulateFulfilmentFailure: true,
    simulateCompensationFailure: true,
  };
  const handle = await DBOS.startWorkflow(OrderLifecycleFulfilmentFailsAfterReservation)(input);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(
    result.status,
    'compensation_failed',
    'a reservation that was held and then failed to release is reported distinctly from a clean unwind',
  );
}

// ---- The approval branch (cleat#2997) ----
//
// These mirror examples/order-lifecycle/order_test.go's four approval tests.
// The third is the one with teeth: an implementation with NO gate at all also
// completes and also passes the first two, so only the timeout case can tell
// a working gate from an absent one.

async function testApprovedOrderProceeds() {
  const handle = await DBOS.startWorkflow(OrderLifecycle)(aboveThreshold('order-approved'));
  await DBOS.send<ApprovalDecision>(handle.workflowID, { approved: true }, DECISION_TOPIC);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(result.status, 'shipped', 'an approved order charges and ships');
  assertEqual(result.stepsCompleted, 3, 'an approved order runs the whole saga');
}

// A rejection must cost nothing. The gate is before the first spending step,
// so there is no compensation trail -- and an implementation that gated AFTER
// the charge would show up here as a non-zero stepsCompleted.
async function testRejectedOrderSpendsNothing() {
  const handle = await DBOS.startWorkflow(OrderLifecycle)(aboveThreshold('order-rejected'));
  await DBOS.send<ApprovalDecision>(handle.workflowID, { approved: false, reason: 'over budget' }, DECISION_TOPIC);
  const result = await handle.getResult();
  assertEqual(result.status, 'rejected', 'a rejected order is reported rejected');
  assertEqual(result.stepsCompleted, 0, 'a rejected order spends nothing');
  assertEqual(await stateOf(handle.workflowID, 'rejection_reason'), 'over budget', 'the reason is published');
}

// THE TEST THAT MATTERS. An unapproved above-threshold order must give up
// rather than proceed. Its window is ONE SECOND rather than the 24h default
// so the timeout is reachable in real time -- cleat's test fast-forwards 24h
// of simulated time instead, which DBOS has no equivalent of; see
// defaultApprovalWindowSeconds in workflow.ts.
async function testAboveThresholdWithoutApprovalDoesNotCharge() {
  const input = { ...aboveThreshold('order-unapproved'), approvalWindowSeconds: 1 };
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);
  // Nothing is sent on the decision topic, so the wait has to time out.
  const result = await handle.getResult();
  assertEqual(result.status, 'rejected', 'an unapproved above-threshold order gives up, not proceeds');
  assertEqual(result.stepsCompleted, 0, 'it never charged');
  const reason = await stateOf(handle.workflowID, 'rejection_reason');
  assertEqual(
    typeof reason === 'string' && reason.indexOf('no approval') !== -1,
    true,
    'a timeout is published with a reason distinct from a refusal',
  );
}

async function testBelowTheThresholdChargesWithoutApproval() {
  // No decision is sent at all: if the gate applied unconditionally the
  // workflow would block on the 24h default and this would never return.
  const handle = await DBOS.startWorkflow(OrderLifecycle)(baseInput('order-below-threshold'));
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(result.status, 'shipped', 'a below-threshold order needs no approval');
}

// The gate has to CLEAR its own status once the decision is in. Without the
// write, an approved order reads "awaiting_approval" for the whole saga --
// through the very steps that charge it. A finished-state assertion cannot
// see that (the terminal write lands either way), so this SAMPLES the
// published status while the run is in flight, exactly as cleat's
// statusesSeen does.
async function testTheGateClearsItsOwnStatus() {
  const input = { ...aboveThreshold('order-status-moves'), approvalWindowSeconds: 30 };
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);

  const seen = new Set<string>();
  let stop = false;
  const sampler = (async () => {
    while (!stop) {
      const s = await stateOf(handle.workflowID, 'status');
      if (typeof s === 'string' && s !== '') seen.add(s);
      await new Promise((r) => setTimeout(r, 5));
    }
  })();

  // Wait for the workflow to actually reach the gate before deciding, so the
  // sample below is of a parked order rather than a not-yet-started one.
  const parked = await waitForState(handle.workflowID, 'status', 'awaiting_approval', 10_000);
  assertEqual(parked, true, 'an above-threshold order publishes awaiting_approval while it waits');

  await DBOS.send<ApprovalDecision>(handle.workflowID, { approved: true }, DECISION_TOPIC);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  await handle.getResult();
  stop = true;
  await sampler;

  assertEqual(seen.has('approved'), true, `the gate cleared its status (saw: ${[...seen].join(', ')})`);
  assertEqual(seen.has('awaiting_approval'), true, 'and it had been published first');
}

async function waitForState(workflowID: string, key: string, want: string, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if ((await stateOf(workflowID, key)) === want) return true;
    await new Promise((r) => setTimeout(r, 5));
  }
  return false;
}

async function main() {
  console.log(`CLAIM: ${CLAIM}`);
  DBOS.setConfig({
    name: 'order-lifecycle-dbos-port-test',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  await DBOS.launch();

  await testHappyPath();
  await testDeclinedNoConfirmation();
  await testReservationFailsAfterCharge();
  await testCompensationItselfFails();
  await testApprovedOrderProceeds();
  await testRejectedOrderSpendsNothing();
  await testAboveThresholdWithoutApprovalDoesNotCharge();
  await testBelowTheThresholdChargesWithoutApproval();
  await testTheGateClearsItsOwnStatus();

  await DBOS.shutdown();

  if (failures > 0) {
    console.error(`${failures} assertion(s) failed`);
    process.exit(1);
  }
  console.log('all assertions passed');
}

main().catch((e) => {
  console.error('test run crashed:', e);
  process.exit(1);
});
