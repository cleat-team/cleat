// Tenant provisioning for a B2B SaaS control plane -- the DBOS half.
//
// The DBOS counterpart to cleat's examples/b2b-saas-control-plane/provision.go,
// at the same scope: a durable provisioning sequence that publishes status as
// it goes, records a milestone per step, provisions a workspace behind a
// placeholder, sends a best-effort welcome email, and a background sweep that
// suspends a tenant whose trial has expired. Written against
// @dbos-inc/dbos-sdk 5.2.11, the current version as of 2026-10-02 -- see the
// pair's README for why the version and the date both matter.
//
// WHAT THIS FILE IS HONEST ABOUT, because the pair exists to show it:
//
//   - **Tenancy is a filter here, not a boundary.** On the cleat side the
//     provisioning run executes AS the tenant it provisions, so every host
//     call it makes is scoped to that tenant by the engine and there is no
//     way for the workflow to address a different one. DBOS has no
//     equivalent: this file receives a tenantId and has to thread it into
//     every write itself. A missed thread is a cross-tenant write that
//     nothing refuses.
//   - **The audit trail is a plain table.** cleat records each milestone
//     through plugins/auditlog: a hash-chained, tenant-scoped table whose
//     append is one transaction that takes a per-tenant head lock. Below,
//     `recordMilestone` is a step that INSERTs into a table this port owns.
//     It is not tamper-evident and nothing verifies it.
//   - **Idempotency is hand-written, and it has to be.** A DBOS step that
//     commits and then dies before its completion is checkpointed WILL be
//     re-run on recovery. That is the same at-least-once hazard cleat's
//     auditlog solves with a deterministic id plus UNIQUE (tenant_id, seq),
//     and the same fix is required here -- see `recordMilestone`. DBOS gives
//     you the retry; it does not give you the dedupe.
import { DBOS } from '@dbos-inc/dbos-sdk';
import { Pool } from 'pg';

export interface ProvisionInput {
  // tenantId is the id the backend already created before this run was
  // started -- the same division the cleat side documents. Supplied rather
  // than derived, and used only to publish and to scope writes; nothing here
  // authorizes anything with it.
  tenantId: string;
  businessName: string;
  adminEmail: string;
  plan: string;
  // simulateWorkspaceFailure makes the workspace placeholder fail, so the
  // failure path is reachable from the browser -- the same reason the cleat
  // side and order-lifecycle both carry such a flag: a scenario that can only
  // show its happy path never needed durability to begin with.
  simulateWorkspaceFailure: boolean;
}

export type ProvisionStatus = 'active' | 'failed';

export interface ProvisionResult {
  tenantId: string;
  status: ProvisionStatus;
  failedStep: string | null;
  welcomeEmailSent: boolean;
  milestonesRecorded: number;
}

// placeholderRoundTripMs stands where the rope-side provisioning call goes --
// seeding a default workspace, a starter project, whatever "ready to use"
// means for your product. DBOS.sleep rather than a bare setTimeout, for the
// reason that matters: DBOS.sleep is checkpointed, so a process that dies
// mid-sleep resumes past it rather than repeating it.
const placeholderRoundTripMs = 120;

// ---- Database ----
//
// One database in play, so one variable: a real deployment would keep its
// application data and DBOS's system database apart, and that separation is
// rope this example does not model. DBOS_SYSTEM_DATABASE_URL is what CI sets
// and what the README's run instructions use.
let pool: Pool | null = null;

function db(): Pool {
  if (pool === null) {
    pool = new Pool({ connectionString: process.env.DBOS_SYSTEM_DATABASE_URL });
  }
  return pool;
}

// ensureSchema is idempotent and called once at launch rather than from the
// workflow, so a provisioning run does not pay for DDL on every step.
export async function ensureSchema(): Promise<void> {
  await db().query(`
    CREATE TABLE IF NOT EXISTS b2b_tenants (
      tenant_id        TEXT PRIMARY KEY,
      business_name    TEXT NOT NULL,
      admin_email      TEXT NOT NULL,
      plan             TEXT NOT NULL,
      api_key          TEXT NOT NULL UNIQUE,
      trial_expires_at TIMESTAMPTZ NOT NULL,
      suspended        BOOLEAN NOT NULL DEFAULT FALSE,
      created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
    )`);
  await db().query(`
    CREATE TABLE IF NOT EXISTS audit_events (
      tenant_id   TEXT NOT NULL,
      event_id    TEXT NOT NULL,
      seq         INTEGER NOT NULL,
      event_type  TEXT NOT NULL,
      details     JSONB NOT NULL DEFAULT '{}'::jsonb,
      recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      PRIMARY KEY (tenant_id, event_id)
    )`);
}

// ---- Audit ----

// recordMilestone appends one row to the calling tenant's audit trail.
//
// eventId is DETERMINISTIC -- workflowId plus the call's sequence number --
// and the insert is ON CONFLICT DO NOTHING against the primary key. That pair
// is what makes this safe to re-run: a step that committed and then died
// before DBOS checkpointed it runs again on recovery, and the second run must
// be a no-op rather than a second row. Ordering is by `seq`, not by
// recorded_at: the clock can tie, the counter cannot.
// Exported for the pair's tests, which assert the dedupe directly by calling
// this twice with the same deterministic event id -- the case a step replay
// produces, and the one that cannot be reached by re-running the workflow
// (DBOS replays a completed step from its checkpoint rather than re-executing
// it, so the hazard only appears at the commit/checkpoint boundary).
export async function recordMilestone(
  tenantId: string,
  workflowId: string,
  seq: number,
  eventType: string,
  details: Record<string, unknown>,
): Promise<void> {
  await db().query(
    `INSERT INTO audit_events (tenant_id, event_id, seq, event_type, details)
     VALUES ($1, $2, $3, $4, $5)
     ON CONFLICT (tenant_id, event_id) DO NOTHING`,
    [tenantId, `${workflowId}:${seq}`, seq, eventType, JSON.stringify(details)],
  );
  DBOS.logger.info(`audit ${eventType} for tenant ${tenantId}`);
}

// ---- Steps ----

async function provisionWorkspace(tenantId: string, fail: boolean): Promise<void> {
  await DBOS.sleep(placeholderRoundTripMs);
  if (fail) {
    throw new Error(`workspace provisioning failed for tenant ${tenantId}`);
  }
  DBOS.logger.info(`workspace provisioned for tenant ${tenantId}`);
}

// Returns a boolean rather than throwing, mirroring cleat's sendWelcomeEmail
// and order-lifecycle's notifyCustomer: a welcome email that does not go out
// must not unwind a tenant that otherwise provisioned cleanly. There is
// nothing to unwind anyway -- nothing here spent anything.
async function sendWelcomeEmail(tenantId: string, adminEmail: string): Promise<boolean> {
  await DBOS.sleep(placeholderRoundTripMs);
  if (adminEmail === '') {
    DBOS.logger.info(`tenant ${tenantId} provisioned, but no admin email was given to welcome`);
    return false;
  }
  DBOS.logger.info(`welcomed ${adminEmail} for tenant ${tenantId}`);
  return true;
}

// ---- The workflow ----
//
// One entry point, matching cleat's single ProvisionTenant: the one
// simulate flag selects which branch runs, the same shape provision.go uses.
//
// The milestone sequence is the cleat side's, event for event:
//   tenant.provisioning_started -> tenant.workspace_provisioned
//     -> tenant.welcome_email_sent -> tenant.provisioning_completed
// with tenant.provisioning_failed replacing the last two on the failure path.
async function provisionTenant(input: ProvisionInput): Promise<ProvisionResult> {
  if (input.tenantId === '') {
    throw new Error('tenantId is required');
  }
  if (input.businessName === '') {
    throw new Error('businessName is required');
  }

  // The workflow id is the run's own identity, and it is what makes each
  // milestone's event id deterministic. DBOS.workflowID is typed optional
  // because the accessor is also callable outside a workflow; inside one it is
  // always set, and a fallback value here would silently key every run's
  // milestones to the same id -- so a missing one is an error, not a default.
  const workflowId = DBOS.workflowID;
  if (workflowId === undefined) {
    throw new Error('provisionTenant ran outside a DBOS workflow: no workflow id to key milestones on');
  }
  let seq = 0;
  let recorded = 0;

  // SETTING the query state is not a step -- it is DBOS's own status surface,
  // the counterpart of cleat's SetQueryState, and it is what the backend
  // polls so a UI can show progress without reading the event history.
  await DBOS.runStep(
    () => recordMilestone(input.tenantId, workflowId, ++seq, 'tenant.provisioning_started', { plan: input.plan }),
    { name: 'recordStarted' },
  );
  recorded++;

  try {
    await DBOS.runStep(
      () => provisionWorkspace(input.tenantId, input.simulateWorkspaceFailure),
      { name: 'provisionWorkspace' },
    );
  } catch {
    // The provisioning failure is the one that matters to the caller. A
    // failure to ALSO record it is logged rather than thrown, so recording
    // does not mask the real error -- the same choice provision.go makes.
    try {
      await DBOS.runStep(
        () =>
          recordMilestone(input.tenantId, workflowId, ++seq, 'tenant.provisioning_failed', {
            step: 'provision_workspace',
          }),
        { name: 'recordFailed' },
      );
      recorded++;
    } catch (recordErr) {
      DBOS.logger.error(
        `tenant ${input.tenantId}: failed to record the provisioning failure -- ${recordErr}`,
      );
    }
    return {
      tenantId: input.tenantId,
      status: 'failed',
      failedStep: 'provision_workspace',
      welcomeEmailSent: false,
      milestonesRecorded: recorded,
    };
  }

  await DBOS.runStep(
    () => recordMilestone(input.tenantId, workflowId, ++seq, 'tenant.workspace_provisioned', {}),
    { name: 'recordWorkspaceProvisioned' },
  );
  recorded++;

  const welcomed = await DBOS.runStep(
    () => sendWelcomeEmail(input.tenantId, input.adminEmail),
    { name: 'sendWelcomeEmail' },
  );
  if (welcomed) {
    await DBOS.runStep(
      () => recordMilestone(input.tenantId, workflowId, ++seq, 'tenant.welcome_email_sent', {}),
      { name: 'recordWelcomeEmailSent' },
    );
    recorded++;
  }

  await DBOS.runStep(
    () => recordMilestone(input.tenantId, workflowId, ++seq, 'tenant.provisioning_completed', {}),
    { name: 'recordCompleted' },
  );
  recorded++;

  DBOS.logger.info(`tenant provisioning complete for ${input.tenantId} (${input.businessName})`);
  return {
    tenantId: input.tenantId,
    status: 'active',
    failedStep: null,
    welcomeEmailSent: welcomed,
    milestonesRecorded: recorded,
  };
}

// ---- The trial-expiry sweep ----
//
// The cleat side's counterpart is plugins/tenantlifecycle's background loop,
// which the scenario script drives by backdating a trial and waiting for the
// real sweep. There is no plugin system here, so the sweep is a DBOS
// scheduled workflow -- and `DBOS.triggerSchedule` fires that same registered
// workflow on demand, which is what the test and the scenario runner use
// instead of waiting for a cron tick. What is skipped in that shortcut is the
// timer, not the sweep: the workflow that runs is the one the schedule runs.
//
// Suspension is the sweep's decision, not the tenant's -- the cleat side's
// tenantlifecycle plugin makes the same point: a workflow setting its OWN
// trial expiry would let a tenant indefinitely postpone the sweep meant to
// constrain it. Nothing in provisionTenant touches trial_expires_at.
async function suspendExpiredTrials(): Promise<void> {
  const workflowId = DBOS.workflowID;
  if (workflowId === undefined) {
    throw new Error('suspendExpiredTrials ran outside a DBOS workflow: no workflow id to key milestones on');
  }
  const { rows } = await db().query<{ tenant_id: string }>(
    `SELECT tenant_id FROM b2b_tenants
      WHERE suspended = FALSE AND trial_expires_at < now()
      ORDER BY tenant_id`,
  );
  for (const row of rows) {
    await db().query(`UPDATE b2b_tenants SET suspended = TRUE WHERE tenant_id = $1`, [row.tenant_id]);
    // seq 0 is reserved for events the SWEEP records, and the event id is
    // keyed on the sweep's own workflow id -- so re-running the sweep for the
    // same tenant is a no-op rather than a second suspension record.
    await DBOS.runStep(
      () => recordMilestone(row.tenant_id, workflowId, 0, 'tenant.trial_expired', {}),
      { name: 'recordTrialExpired' },
    );
    DBOS.logger.info(`suspended tenant ${row.tenant_id}: trial expired`);
  }
}

export const ProvisionTenant = DBOS.registerWorkflow(provisionTenant, { name: 'provisionTenant' });
export const SuspendExpiredTrials = DBOS.registerWorkflow(suspendExpiredTrials, {
  name: 'suspendExpiredTrials',
});

// registerSweepSchedule wires the sweep to a cron. Called after DBOS.launch()
// by both the server and the test bootstrap; `triggerSchedule` on the same
// name is how a caller runs it without waiting.
export async function registerSweepSchedule(): Promise<void> {
  await DBOS.createSchedule({
    scheduleName: 'trial-expiry-sweep',
    workflowFn: SuspendExpiredTrials,
    // Every minute: a demo-scale cadence. The scenario runner and the tests
    // trigger the schedule explicitly rather than waiting for it.
    schedule: '* * * * *',
  });
}
