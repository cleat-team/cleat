// HTTP backend for the order-lifecycle DBOS port. The DBOS counterpart to
// examples/order-lifecycle/backend/main.go: an endpoint to place an order, an
// endpoint the payment provider's webhook calls, an endpoint to deliver the
// human approval decision, and a status read that returns the order's published
// query state.
//
// The approval endpoint and the query state were added by cleat#2997; before
// that this backend had four routes and no decision path at all, while the
// cleat side's had five plus published state.
//
// TENANCY: 0 platform lines, same as cleat's HTTP layer, but for the opposite
// reason. Cleat's worker enforces row-level tenant isolation underneath every
// request; this backend enforces NOTHING -- there is one flat DBOS
// application namespace, and a tenant filter would be a WHERE clause this
// file would have to write and a boundary an attacker could omit by mistake.
// The measurement doc's "tenancy: 17 (a filter, not a boundary)" line is
// about exactly this: a filter is application code, a boundary is the
// platform's job, and DBOS does not do the platform's job here.
import express from 'express';
import { DBOS } from '@dbos-inc/dbos-sdk';
import {
  ApprovalDecision,
  DECISION_TOPIC,
  OrderLifecycle,
  OrderLifecycleFulfilmentFailsAfterReservation,
  OrderInput,
  OrderResult,
} from './workflow';

const app = express();
app.use(express.json());

interface PlaceOrderBody {
  orderId: string;
  customerId: string;
  email: string;
  items: { sku: string; quantity: number; priceCents: number }[];
  simulatePaymentFailure?: boolean;
  simulateFulfilmentFailure?: boolean;
  simulateCompensationFailure?: boolean;
  // failAfterReservation selects the second workflow, whose simulated
  // failure lands after a real reservation rather than at it -- see
  // workflow.ts's comment on why that needs a second entry point.
  failAfterReservation?: boolean;
  // approvalWindowSeconds overrides the 24h default an above-threshold order
  // waits for a decision. It is here so the timeout path is demonstrable
  // without waiting a day; see workflow.ts's defaultApprovalWindowSeconds.
  approvalWindowSeconds?: number;
}

// A dedicated readiness endpoint, rather than reusing /orders for that
// purpose: POST /orders starts a real, durably-recorded workflow on every
// call, so probing readiness against it would leave scratch workflow rows
// behind for every retry a slow-starting server needs.
app.get('/healthz', (_req, res) => {
  res.status(200).json({ ok: true });
});

app.post('/orders', async (req, res) => {
  const body = req.body as PlaceOrderBody;
  const input: OrderInput = {
    orderId: body.orderId,
    customerId: body.customerId,
    email: body.email,
    items: body.items,
    simulatePaymentFailure: body.simulatePaymentFailure ?? false,
    simulateFulfilmentFailure: body.simulateFulfilmentFailure ?? false,
    simulateCompensationFailure: body.simulateCompensationFailure ?? false,
    approvalWindowSeconds: body.approvalWindowSeconds,
  };
  const workflow = body.failAfterReservation ? OrderLifecycleFulfilmentFailsAfterReservation : OrderLifecycle;
  const handle = await DBOS.startWorkflow(workflow)(input);
  res.status(202).json({ orderId: input.orderId, workflowID: handle.workflowID });
});

// The PSP's payment-confirmation webhook. A real deployment authenticates
// this with the provider's signature; that is rope, not cleat -- the
// measurement doc's "rope side is a placeholder" note applies here exactly
// as it does on the cleat side.
app.post('/webhooks/payment/:orderWorkflowId', async (req, res) => {
  const { orderWorkflowId } = req.params;
  await DBOS.send<string>(orderWorkflowId, 'confirmed', 'payment-confirmed');
  res.status(200).json({ ok: true });
});

// The human approval decision -- cleat's backend/main.go has the same route
// and sends one of two signal NAMES ("order_approved"/"order_rejected"). Here
// it is ONE topic carrying a payload that names the decision, because DBOS
// has no multi-signal wait; see DECISION_TOPIC in workflow.ts.
app.post('/orders/:workflowId/approve', async (req, res) => {
  const { workflowId } = req.params;
  const body = req.body as { approve?: boolean; reason?: string };
  const approved = body.approve !== false;
  const decision: ApprovalDecision = {
    approved,
    // A refusal with no reason gets the same placeholder cleat's backend
    // substitutes, so the two sides read alike.
    reason: approved ? undefined : body.reason || 'rejected without a reason',
  };
  await DBOS.send<ApprovalDecision>(workflowId, decision, DECISION_TOPIC);
  res.status(200).json({ decision: approved ? 'approved' : 'rejected' });
});

// STATE_KEYS are the query-state keys the workflow publishes. They are read
// with an explicit zero timeout: getEvent's default is to WAIT (60s) for the
// event to appear, and a status endpoint must not block on a key an order
// that never reached that stage will never set.
const STATE_KEYS = ['order_id', 'status', 'total_cents', 'rejection_reason'] as const;

async function readState(workflowID: string): Promise<Record<string, unknown>> {
  const state: Record<string, unknown> = {};
  for (const key of STATE_KEYS) {
    const value = await DBOS.getEvent<unknown>(workflowID, key, { timeoutSeconds: 0 });
    if (value !== null && value !== undefined) {
      state[key] = value;
    }
  }
  return state;
}

// DBOS's own status field (PENDING, SUCCESS, ERROR, ...) is the workflow's
// LIFECYCLE state, not the business outcome order.ts's saga computes --
// those are different claims, the same distinction the workflow file draws
// between "the compensation ran" and "the compensation worked". A caller
// polling for the order's fate wants the latter, so this reports the
// business status once the workflow has one and the lifecycle status while
// it is still in flight, rather than conflating the two under one name.
//
// `state` is the third thing and is kept separate from both, exactly as
// cleat's backend keeps it: the workflow's PUBLISHED query state, which is
// where an in-flight order's progress lives. An order parked on its approval
// gate reads status:"awaiting_approval" there and moves on without the caller
// reading the event history -- and note that `status` above deliberately does
// NOT take that value, because a poller watching `status` is waiting for the
// order's fate, not its progress.
app.get('/orders/:workflowId', async (req, res) => {
  const workflowID = req.params.workflowId;
  const handle = DBOS.retrieveWorkflow<OrderResult>(workflowID);
  const dbosStatus = (await handle.getStatus())?.status ?? 'UNKNOWN';
  const state = await readState(workflowID);
  if (dbosStatus === 'SUCCESS') {
    const result = await handle.getResult();
    res.status(200).json({ status: result.status, dbosStatus, state, result });
    return;
  }
  res.status(200).json({ status: dbosStatus, dbosStatus, state, result: null });
});

export async function main() {
  DBOS.setConfig({
    name: 'order-lifecycle-dbos-port',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  await DBOS.launch();
  const port = Number(process.env.PORT ?? 3000);
  app.listen(port, () => {
    console.log(`order-lifecycle DBOS port listening on :${port}`);
  });
}

if (require.main === module) {
  main().catch((e) => {
    console.error('failed to start:', e);
    process.exit(1);
  });
}
