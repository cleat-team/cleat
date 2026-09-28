// HTTP backend for the order-lifecycle DBOS port. The DBOS counterpart to
// examples/order-lifecycle/backend/main.go, at the same scope: an endpoint to
// place an order, an endpoint the payment provider's webhook calls, and a
// status read.
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

// DBOS's own status field (PENDING, SUCCESS, ERROR, ...) is the workflow's
// LIFECYCLE state, not the business outcome order.ts's saga computes --
// those are different claims, the same distinction the workflow file draws
// between "the compensation ran" and "the compensation worked". A caller
// polling for the order's fate wants the latter, so this reports the
// business status once the workflow has one and the lifecycle status while
// it is still in flight, rather than conflating the two under one name.
app.get('/orders/:workflowId', async (req, res) => {
  const handle = DBOS.retrieveWorkflow<OrderResult>(req.params.workflowId);
  const dbosStatus = (await handle.getStatus())?.status ?? 'UNKNOWN';
  if (dbosStatus === 'SUCCESS') {
    const result = await handle.getResult();
    res.status(200).json({ status: result.status, dbosStatus, result });
    return;
  }
  res.status(200).json({ status: dbosStatus, dbosStatus, result: null });
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
