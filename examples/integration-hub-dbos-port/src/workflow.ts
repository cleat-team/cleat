// The wedge, DBOS side -- cleat#2597's second cleat-vs-DBOS pair. See
// README.md for the finding this pair exists to measure BEFORE reading any
// code here: DBOS has no primitive, documented anywhere in current
// docs.dbos.dev, for accepting a tenant's own code at runtime and executing
// it -- sandboxed or not. What follows is the closest a DBOS application can
// come today: a plain function, already part of this file at compile time,
// composed into a workflow the only way DBOS composes any step
// (DBOS.runStep). That narrowing -- no upload path, no dynamically loaded
// module -- is not a shortcut taken while porting; it is the measurement.
// Building an upload endpoint here would attribute infrastructure this
// author wrote to a platform capability DBOS does not have.
//
// Written against @dbos-inc/dbos-sdk 5.2.11, which all three DBOS ports now
// pin (cleat#2955). This comment said 5.1.10 -- the version current when the
// port was written, 2026-09-28 -- and closed by asking a later reader to
// "re-check both before trusting either". That instruction is what caught it,
// one pass later than it should have: a sweep for the old pin searched only
// *.md, so this file, which is not markdown, was missed by the correction
// that was supposed to be complete.
import { DBOS } from '@dbos-inc/dbos-sdk';
import * as fs from 'node:fs';

export interface NormalizeOrderInput {
  orderId: string;
  vendorName?: string;
}

export interface NormalizeOrderResult {
  orderId: string;
  normalized: true;
  transformed: string;
}

// normalizeOrder -- the LEGITIMATE tenant step, at the same scope as
// examples/integration-hub/tenant-steps/normalize-order/main.go: it does
// nothing host-privileged, because an ordinary tenant transform needs none
// of the access this pair is about.
//
// ITS ROLE HERE IS THE MANDATORY POSITIVE CONTROL. Without a legitimate step
// that demonstrably succeeds, a failure on readHostFile below would be
// indistinguishable from a DBOS runtime that fails every step -- the same
// requirement the cleat-side sandbox scenario carries
// (scripts/run-integration-hub-tenant-sandbox-scenario.sh's own header
// comment: "a FAIL on the adversarial arm alone would be worthless").
async function normalizeOrder(input: NormalizeOrderInput): Promise<NormalizeOrderResult> {
  if (!input.orderId) {
    throw new Error('order_id is required');
  }
  DBOS.logger.info(`normalizing order ${input.orderId} from ${input.vendorName ?? 'unknown vendor'}`);
  return { orderId: input.orderId, normalized: true, transformed: 'normalize-order v1' };
}

// readHostFile -- the ADVERSARIAL probe, matching the INTENT of
// examples/integration-hub/tenant-steps/malicious-read-host-file/main.go
// exactly: the simplest thing a tenant-supplied step could do to reach the
// host filesystem. Under cleat this is a hard trap
// (engine/wasi_policy.go lists path_open as wasiFatal). Under DBOS this
// SUCCEEDS: a DBOS step is a plain function running in the same Node
// process as the rest of the application, so there is no boundary between
// "code DBOS is running on a tenant's behalf" and "code the operator wrote"
// for anything to enforce a refusal against.
//
// THIS SUCCEEDING IS THE FINDING, NOT A BUG IN THIS FILE. See README.md's
// "Why a trivially-true result is evidence about DBOS" before reading this
// as merely "of course Node can read a file" -- that reading is correct and
// beside the point that this pair measures.
// /etc/hosts, not /etc/hostname: the latter does not exist on macOS, and a
// missing-file ENOENT there is indistinguishable from a refusal without
// reading the message -- measured while writing this port. /etc/hosts
// exists on every POSIX host this test runs on, in CI (ubuntu-latest) and
// locally alike.
async function readHostFile(): Promise<string> {
  return fs.readFileSync('/etc/hosts', 'utf8');
}

// runTenantStep -- the workflow, matching examples/integration-hub/hub.go's
// SyncCustomer dispatching to h.ChildWorkflow(in.TenantStepName, ...): a
// caller names which "tenant step" to run, and the workflow runs it as a
// step of itself.
//
// THE DIFFERENCE FROM CLEAT'S SIDE IS WHAT THIS PAIR IS ABOUT. TenantStepName
// there resolves a WASM module a TENANT uploaded through
// POST /api/definitions, sandboxed by the engine before it ever runs.
// stepName here resolves one of THIS FILE's own functions, chosen at
// compile time by the operator -- there is no tenant-uploaded code on this
// side of the pair, because DBOS has nothing that would run it if there
// were. Read this as "which of the operator's own functions to call", not
// as an equivalent to cleat's dispatch.
export type TenantStepName = 'normalize-order' | 'read-host-file';

async function runTenantStep(stepName: TenantStepName, input: NormalizeOrderInput): Promise<unknown> {
  switch (stepName) {
    case 'normalize-order':
      return DBOS.runStep(() => normalizeOrder(input), { name: 'normalizeOrder' });
    case 'read-host-file':
      return DBOS.runStep(() => readHostFile(), { name: 'readHostFile' });
  }
}

export const RunTenantStep = DBOS.registerWorkflow(runTenantStep, { name: 'runTenantStep' });
