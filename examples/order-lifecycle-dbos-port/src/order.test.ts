// Executed assertions against a real DBOS runtime and a real Postgres -- not
// a description of expected behaviour. Run with `npm run build && npm test`
// against DBOS_SYSTEM_DATABASE_URL. No test framework: four scenarios, each
// asserting the OUTCOME a workflow PUBLISHED, mirroring the shape
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
// If a future cleat feature (item (a) in the 0.4.0 levers list: a blocking
// durable wait for webhooks/signals, replacing the found:false polling loop)
// changes which side is smaller, this pair's job is unaffected: it does not
// assert a winner, it asserts that both sides are real and comparable. See
// the pair's README for the cloc numbers as measured on 2026-09-28.
import { DBOS } from '@dbos-inc/dbos-sdk';
import {
  OrderLifecycle,
  OrderLifecycleFulfilmentFailsAfterReservation,
  OrderInput,
} from './workflow';

const CLAIM =
  'order-lifecycle-dbos-port: a genuinely executed, equally-scoped DBOS ' +
  "counterpart exists and is comparable to cleat's by the same line counter " +
  '-- this pair asserts comparability, not a winner (cleat#2597).';

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

let failures = 0;

function assertEqual<T>(got: T, want: T, why: string) {
  if (got !== want) {
    failures++;
    console.error(`FAIL: ${why} -- got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  } else {
    console.log(`ok: ${why}`);
  }
}

async function testHappyPath() {
  const input = baseInput('order-happy');
  const handle = await DBOS.startWorkflow(OrderLifecycle)(input);
  await DBOS.send<string>(handle.workflowID, 'confirmed', 'payment-confirmed');
  const result = await handle.getResult();
  assertEqual(result.status, 'shipped', 'a confirmed payment and a clean reservation ships');
  assertEqual(result.stepsCompleted, 3, 'all three steps ran');
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
