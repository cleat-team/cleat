// HTTP backend for the b2b-saas-control-plane DBOS port. The DBOS counterpart
// to examples/b2b-saas-control-plane/backend/main.go, at the same scope: sign
// a business up, start its provisioning run, poll that run's status, and let
// a tenant read its OWN lifecycle row.
//
// TENANCY: this file enforces tenancy, and that is the finding. On the cleat
// side the equivalent route is scoped by the platform -- the worker resolves
// the caller's tenant from the API key that authenticated the request, and
// every tenant-scoped table has row-level security FORCEd underneath, so a
// caller cannot address another tenant's row even by writing the query wrong.
// Here, `GET /api/tenant/lifecycle` works because this file looks the
// caller's tenant up in `b2b_tenants` and puts that id in a WHERE clause. A
// filter is application code; a boundary is the platform's job. DBOS does not
// do the platform's job here, and the cross-tenant test asserts the filter
// holds rather than that something refuses.
//
// No web UI, matching the sibling ports: examples/order-lifecycle has a web/
// directory and examples/order-lifecycle-dbos-port does not, and this pair
// follows that precedent rather than inventing a difference.
import express from 'express';
import { randomBytes, randomUUID } from 'crypto';
import { DBOS } from '@dbos-inc/dbos-sdk';
import { Pool } from 'pg';
import {
  ProvisionTenant,
  ProvisionInput,
  ProvisionResult,
  ensureSchema,
  registerSweepSchedule,
} from './workflow';

const app = express();
app.use(express.json());

let pool: Pool | null = null;

function db(): Pool {
  if (pool === null) {
    pool = new Pool({ connectionString: process.env.DBOS_SYSTEM_DATABASE_URL });
  }
  return pool;
}

// A dedicated readiness endpoint rather than reusing /api/signup: signing a
// business up creates a tenant row AND starts a durably-recorded workflow, so
// probing readiness against it would leave scratch tenants behind for every
// retry a slow-starting server needs.
app.get('/healthz', (_req, res) => {
  res.status(200).json({ ok: true });
});

interface SignupBody {
  businessName: string;
  adminEmail: string;
  plan?: string;
  trialDays?: number;
  simulateWorkspaceFailure?: boolean;
}

// signup is the operator's own automation: it creates the tenant and mints
// its key BEFORE any run starts, then starts the run AS that tenant. The
// division is deliberate and mirrors the cleat side -- a workflow cannot
// create the tenant it is about to run as, and a workflow that could set its
// own trial expiry would let a tenant postpone the sweep meant to constrain
// it. Creating a tenant is a privileged, out-of-band action on both sides;
// on cleat it is `cleat-worker --create-tenant`, here it is this INSERT.
app.post('/api/signup', async (req, res) => {
  const body = req.body as SignupBody;
  if (!body.businessName || !body.adminEmail) {
    res.status(400).json({ error: 'businessName and adminEmail are required' });
    return;
  }
  const tenantId = randomUUID();
  const apiKey = randomBytes(24).toString('hex');
  const trialDays = body.trialDays ?? 14;

  await db().query(
    `INSERT INTO b2b_tenants (tenant_id, business_name, admin_email, plan, api_key, trial_expires_at)
     VALUES ($1, $2, $3, $4, $5, now() + ($6 || ' days')::interval)`,
    [tenantId, body.businessName, body.adminEmail, body.plan ?? 'starter', apiKey, String(trialDays)],
  );

  const input: ProvisionInput = {
    tenantId,
    businessName: body.businessName,
    adminEmail: body.adminEmail,
    plan: body.plan ?? 'starter',
    simulateWorkspaceFailure: body.simulateWorkspaceFailure ?? false,
  };
  const handle = await DBOS.startWorkflow(ProvisionTenant)(input);
  res.status(202).json({ tenantId, workflowID: handle.workflowID, apiKey });
});

// DBOS's own status field (PENDING, SUCCESS, ERROR, ...) is the workflow's
// LIFECYCLE state, not the business outcome this port computes -- the same
// distinction the sibling port's server.ts draws. A caller polling for the
// tenant's fate wants the latter, so this reports the business status once
// the workflow has one and the lifecycle status while it is in flight.
app.get('/api/provisioning/:workflowId', async (req, res) => {
  const handle = DBOS.retrieveWorkflow<ProvisionResult>(req.params.workflowId);
  const dbosStatus = (await handle.getStatus())?.status ?? 'UNKNOWN';
  if (dbosStatus === 'SUCCESS') {
    const result = await handle.getResult();
    res.status(200).json({ status: result.status, dbosStatus, result });
    return;
  }
  res.status(200).json({ status: dbosStatus, dbosStatus, result: null });
});

// The tenant's own suspended/trial state. Tenant-scoped by the WHERE clause
// below -- see the file header for why that sentence is the finding rather
// than a detail.
app.get('/api/tenant/lifecycle', async (req, res) => {
  const auth = req.header('authorization') ?? '';
  const key = auth.startsWith('Bearer ') ? auth.slice('Bearer '.length) : '';
  if (key === '') {
    res.status(401).json({ error: 'a Bearer token is required' });
    return;
  }
  const { rows } = await db().query(
    `SELECT tenant_id, business_name, plan, suspended, trial_expires_at
       FROM b2b_tenants WHERE api_key = $1`,
    [key],
  );
  if (rows.length === 0) {
    // One response for "no such key" and "a key that is not yours": a
    // distinguishable answer would make this route an existence oracle for
    // other tenants' keys.
    res.status(404).json({ error: 'no tenant for that token' });
    return;
  }
  res.status(200).json({ tenant: rows[0] });
});

export async function main() {
  DBOS.setConfig({
    name: 'b2b-saas-control-plane-dbos-port',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  // DBOS.launch() FIRST, and this ordering is load-bearing: it creates the
  // system database if the URL names one that does not exist yet, which is
  // how the CI job gets away with no create-database step. Measured -- a pool
  // opened against a missing database fails with `3D000`, so calling
  // ensureSchema() before launch works locally (where the database already
  // exists) and fails on a fresh runner.
  await DBOS.launch();
  await ensureSchema();
  await registerSweepSchedule();
  const port = Number(process.env.PORT ?? 3000);
  app.listen(port, () => {
    console.log(`b2b-saas-control-plane DBOS port listening on :${port}`);
  });
}

if (require.main === module) {
  main().catch((e) => {
    console.error('failed to start:', e);
    process.exit(1);
  });
}
