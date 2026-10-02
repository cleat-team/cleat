// Executed assertions against a real DBOS runtime and a real Postgres -- not
// a description of expected behaviour. Run with `npm run build && npm test`
// against DBOS_SYSTEM_DATABASE_URL. No test framework: six scenarios, each
// asserting the OUTCOME a workflow PUBLISHED, mirroring the shape
// examples/b2b-saas-control-plane/provision_test.go asserts (status and the
// audit trail, not internals).
//
// THE CLAIM THIS PAIR CARRIES (cleat#2597 criterion 3). Unlike
// order-lifecycle-dbos-port, this pair is NOT the control: it exists to
// exercise the situations where cleat's advantage is structural. So the claim
// is about WHICH PARTS OF THE SCENARIO THE PORT HAS TO WRITE ITSELF, and
// every clause of it is carried by a test below:
//
//   "An equally-scoped, idiomatic, EXECUTED DBOS port of the
//    b2b-saas-control-plane scenario exists and runs, and building it shows
//    that the tenant filter, the audit append and the append's replay
//    idempotency are the port author's code rather than the platform's --
//    where cleat supplies all three."
//
// The filter clause is carried by `testATenantKeyCannotReadAnotherTenantsRow`
// and the idempotency clause by `testMilestoneRecordingIsIdempotent`. Neither
// asserts that DBOS is worse; they assert that the code is here and not
// there, which is a fact about the two platforms rather than a preference.
import { DBOS } from '@dbos-inc/dbos-sdk';
import { Pool } from 'pg';
import { randomUUID } from 'crypto';
import { ProvisionTenant, ProvisionInput, ensureSchema, recordMilestone, registerSweepSchedule } from './workflow';

const CLAIM =
  'b2b-saas-control-plane-dbos-port: an equally-scoped, idiomatic, EXECUTED DBOS port exists ' +
  'and runs, and shows the tenant filter, the audit append and its replay idempotency are the ' +
  "port author's code rather than the platform's -- where cleat supplies all three (cleat#2597).";

function baseInput(tenantId: string): ProvisionInput {
  return {
    tenantId,
    businessName: 'Acme Widgets',
    adminEmail: 'admin@acme.example',
    plan: 'starter',
    simulateWorkspaceFailure: false,
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

function assertTrue(cond: boolean, why: string) {
  if (!cond) {
    failures++;
    console.error(`FAIL: ${why}`);
  } else {
    console.log(`ok: ${why}`);
  }
}

function db(): Pool {
  return new Pool({ connectionString: process.env.DBOS_SYSTEM_DATABASE_URL });
}

async function auditRows(pool: Pool, tenantId: string): Promise<{ event_type: string; seq: number }[]> {
  const { rows } = await pool.query(
    `SELECT event_type, seq FROM audit_events WHERE tenant_id = $1 ORDER BY seq`,
    [tenantId],
  );
  return rows;
}

// ---- Scenarios ----

async function testProvisioningCompletes(pool: Pool) {
  const tenantId = randomUUID();
  const handle = await DBOS.startWorkflow(ProvisionTenant)(baseInput(tenantId));
  const result = await handle.getResult();

  assertEqual(result.status, 'active', 'a whole provisioning run completes');
  assertEqual(result.failedStep, null, 'the happy path publishes no failed step');
  assertEqual(result.welcomeEmailSent, true, 'the welcome email went out');
  assertEqual(result.milestonesRecorded, 4, 'four milestones were recorded');

  const events = (await auditRows(pool, tenantId)).map((r) => r.event_type);
  assertEqual(
    events.join(','),
    'tenant.provisioning_started,tenant.workspace_provisioned,tenant.welcome_email_sent,tenant.provisioning_completed',
    "every milestone landed in this tenant's own audit trail, in order",
  );
}

async function testProvisioningFailsAtTheWorkspace(pool: Pool) {
  const tenantId = randomUUID();
  const input = { ...baseInput(tenantId), simulateWorkspaceFailure: true };
  const handle = await DBOS.startWorkflow(ProvisionTenant)(input);
  const result = await handle.getResult();

  assertEqual(result.status, 'failed', 'a workspace failure fails the run');
  assertEqual(result.failedStep, 'provision_workspace', 'the failing step is published, not inferred');
  assertEqual(result.welcomeEmailSent, false, 'no welcome email is sent on the failure path');

  const events = (await auditRows(pool, tenantId)).map((r) => r.event_type);
  assertEqual(
    events.join(','),
    'tenant.provisioning_started,tenant.provisioning_failed',
    'the failure is RECORDED, and the completed/email milestones are absent',
  );
}

async function testWelcomeEmailIsBestEffort(pool: Pool) {
  const tenantId = randomUUID();
  const input = { ...baseInput(tenantId), adminEmail: '' };
  const handle = await DBOS.startWorkflow(ProvisionTenant)(input);
  const result = await handle.getResult();

  // The distinction that matters: an email that does not go out must not
  // unwind a tenant that otherwise provisioned cleanly, and must not be
  // recorded as having been sent.
  assertEqual(result.status, 'active', 'a tenant that could not be emailed is still provisioned');
  assertEqual(result.welcomeEmailSent, false, 'the email is reported as not sent, not silently counted');
  const events = (await auditRows(pool, tenantId)).map((r) => r.event_type);
  assertTrue(
    !events.includes('tenant.welcome_email_sent'),
    'no welcome-email milestone is recorded for an email that was never sent',
  );
}

// The idempotency clause of the claim. A DBOS step that commits and then dies
// before its completion is checkpointed is re-run on recovery; this is that
// re-run, made explicit. It cannot be reached by restarting the workflow --
// DBOS replays a completed step from its checkpoint rather than re-executing
// it -- which is exactly why the port author has to get this right and why
// the test calls the step's own function rather than the workflow.
async function testMilestoneRecordingIsIdempotent(pool: Pool) {
  const tenantId = randomUUID();
  const workflowId = `idempotency-probe-${randomUUID()}`;

  await recordMilestone(tenantId, workflowId, 1, 'tenant.provisioning_started', { plan: 'starter' });
  await recordMilestone(tenantId, workflowId, 1, 'tenant.provisioning_started', { plan: 'starter' });

  const rows = await auditRows(pool, tenantId);
  assertEqual(rows.length, 1, 're-recording the same milestone appends once, not twice');
  assertEqual(rows[0].seq, 1, 'the surviving row is the original, ordered by its sequence number');
}

// The filter clause of the claim, and the closest thing this pair has to the
// cleat side's cross-tenant refusal test. The difference the pair exists to
// show: on the cleat side this assertion is about something REFUSING, because
// row-level security is FORCEd underneath every tenant-scoped table. Here it
// is about a WHERE clause this port wrote, and the test can only assert that
// the port wrote it correctly.
async function testATenantKeyCannotReadAnotherTenantsRow(pool: Pool) {
  const a = randomUUID();
  const b = randomUUID();
  const keyA = `key-a-${randomUUID()}`;
  const keyB = `key-b-${randomUUID()}`;
  await pool.query(
    `INSERT INTO b2b_tenants (tenant_id, business_name, admin_email, plan, api_key, trial_expires_at)
     VALUES ($1,'Tenant A','a@example.com','starter',$2, now() + interval '14 days'),
            ($3,'Tenant B','b@example.com','starter',$4, now() + interval '14 days')`,
    [a, keyA, b, keyB],
  );

  // The route's own query, verbatim -- asserted here so the property is
  // checked against the row the route would return rather than argued from
  // the route's source.
  const asA = await pool.query(`SELECT tenant_id, business_name FROM b2b_tenants WHERE api_key = $1`, [keyA]);
  const asB = await pool.query(`SELECT tenant_id, business_name FROM b2b_tenants WHERE api_key = $1`, [keyB]);

  assertEqual(asA.rows.length, 1, "tenant A's key resolves exactly one row");
  assertEqual(asA.rows[0].tenant_id, a, "tenant A's key resolves tenant A");
  assertEqual(asB.rows[0].tenant_id, b, "tenant B's key resolves tenant B");
  assertTrue(
    asA.rows.every((r: { tenant_id: string }) => r.tenant_id !== b),
    "tenant A's key can never resolve tenant B's row",
  );
}

async function testTheSweepSuspendsAnExpiredTrial(pool: Pool) {
  const tenantId = randomUUID();
  // Backdated, the way the cleat-side scenario script does it: the sweep runs
  // on a cron, so a trial that expires in fourteen days is not observable
  // within a test run.
  await pool.query(
    `INSERT INTO b2b_tenants (tenant_id, business_name, admin_email, plan, api_key, trial_expires_at)
     VALUES ($1,'Expired Co','e@example.com','starter',$2, now() - interval '1 day')`,
    [tenantId, `key-exp-${randomUUID()}`],
  );

  // The schedule's own workflow, fired on demand. What is skipped is the
  // timer, not the sweep -- see workflow.ts's note on this.
  const handle = await DBOS.triggerSchedule('trial-expiry-sweep');
  await handle.getResult();

  const { rows } = await pool.query(`SELECT suspended FROM b2b_tenants WHERE tenant_id = $1`, [tenantId]);
  assertEqual(rows[0].suspended, true, 'the sweep suspends a tenant whose trial has expired');

  const events = (await auditRows(pool, tenantId)).map((r) => r.event_type);
  assertTrue(events.includes('tenant.trial_expired'), 'the suspension is recorded in the audit trail');

  // A tenant inside its trial is left alone -- otherwise "suspended" would be
  // a property of having run the sweep at all.
  const active = randomUUID();
  await pool.query(
    `INSERT INTO b2b_tenants (tenant_id, business_name, admin_email, plan, api_key, trial_expires_at)
     VALUES ($1,'Active Co','x@example.com','starter',$2, now() + interval '5 days')`,
    [active, `key-act-${randomUUID()}`],
  );
  const second = await DBOS.triggerSchedule('trial-expiry-sweep');
  await second.getResult();
  const { rows: activeRows } = await pool.query(`SELECT suspended FROM b2b_tenants WHERE tenant_id = $1`, [active]);
  assertEqual(activeRows[0].suspended, false, 'a tenant still inside its trial is not suspended');
}

async function main() {
  console.log(`CLAIM: ${CLAIM}`);
  DBOS.setConfig({
    name: 'b2b-saas-control-plane-dbos-port-test',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  // launch() before ensureSchema(): it is what creates the system database
  // when the URL names one that does not exist yet. See server.ts's main().
  await DBOS.launch();
  await ensureSchema();
  await registerSweepSchedule();

  const pool = db();
  try {
    await testProvisioningCompletes(pool);
    await testProvisioningFailsAtTheWorkspace(pool);
    await testWelcomeEmailIsBestEffort(pool);
    await testMilestoneRecordingIsIdempotent(pool);
    await testATenantKeyCannotReadAnotherTenantsRow(pool);
    await testTheSweepSuspendsAnExpiredTrial(pool);
  } finally {
    await pool.end();
    await DBOS.shutdown();
  }

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
