// The wedge, DBOS side, SECOND counterpart -- cleat#2597, added after
// cleat-review's review of the first: bare DBOS.runStep (workflow.ts) shows
// what happens if a team does nothing special, and that comparison is
// one-sided. A team actually shipping tenant-supplied code on DBOS would not
// do that; they would run it in an isolating runtime or service --
// "orchestrator plus a separate sandbox", in the positioning memo's words.
// This file is that counterpart: DBOS.runStep wrapping an execution inside
// isolated-vm, an in-process V8 isolate library, rather than a plain
// function call.
//
// WHY isolated-vm, SPECIFICALLY. It is the lowest-friction idiomatic choice
// for a Node/TypeScript team that wants to run untrusted JavaScript without
// standing up a container fleet or a separate sandboxing service: no extra
// infrastructure, no network hop, just a library dependency. A V8 isolate
// has its OWN heap and, critically, starts with NO access to Node's built-in
// modules -- `require`, `fs`, `process`, network sockets -- unless the host
// code explicitly injects a reference into the isolate's global object. That
// is a REAL boundary, unlike the bare-step version: verified empirically
// below (isolated-wedge.test.ts), not merely asserted.
//
// isolated-vm@6.1.2 on Node 24 ("Krypton", the current LTS line as of
// 2026-09-28) -- both pinned together and both verified empirically in this
// PR, not chosen from a changelog. isolated-vm@7.x requires Node >=26,
// which is still the bleeding-edge "Current" release, not LTS; an idiomatic
// team pins its sandbox library to the version its LTS runtime actually
// supports, so 6.1.2 (Node >=22) is the more defensible choice, and CI's
// node-version is pinned to 24 for the same reason. 6.1.2 ships prebuilt
// native binaries for darwin-arm64 (`node_modules/isolated-vm/prebuilds/`),
// so nothing here needs a local C++ toolchain to build. Unlike
// @dbos-inc/dbos-sdk, this dependency is NOT part of DBOS -- criterion 1's
// "idiomatic, current docs" bar applies to the DBOS SDK; this is the
// "separate sandbox" the owner's framing asks the counterpart to include, so
// its version is tracked here for the same reason (reproducibility), not
// because DBOS documents it.
//
// THE TENANT'S CODE IS A STRING HERE, NOT A FUNCTION REFERENCE -- and that
// is the point of this file over workflow.ts's runTenantStep. A function
// already compiled into this file at build time is not what a tenant
// uploads; a string evaluated at runtime inside an isolate is the closest
// idiomatic DBOS analogue to cleat's POST /api/definitions payload. It is
// still not a full analogue -- nothing here PERSISTS an uploaded string
// (see README.md, "What this still does not build") -- but it is honestly
// closer than a hardcoded function.
import { DBOS } from '@dbos-inc/dbos-sdk';
import ivm from 'isolated-vm';

export interface NormalizeOrderInput {
  orderId: string;
  vendorName?: string;
}

// The same two tenant behaviours as workflow.ts, expressed as SOURCE TEXT --
// standing in for what a tenant uploaded -- rather than as TypeScript
// functions compiled into this file. Deliberately near-identical logic to
// workflow.ts's normalizeOrder/readHostFile, so the only variable between
// the two counterparts is the execution boundary, not the tenant logic.
const NORMALIZE_ORDER_SOURCE = `
  (function (inputJSON) {
    const input = JSON.parse(inputJSON);
    if (!input.order_id) {
      throw new Error('order_id is required');
    }
    return JSON.stringify({
      order_id: input.order_id,
      normalized: true,
      transformed: 'normalize-order v1',
    });
  })
`;

// Attempts the exact same host-filesystem read as workflow.ts's
// readHostFile -- via Node's require('fs'), which is the naive thing a
// tenant's code would try. Inside a fresh isolate, require does not exist:
// this throws ReferenceError before it ever reaches the filesystem.
const READ_HOST_FILE_SOURCE = `
  (function () {
    const fs = require('fs');
    return fs.readFileSync('/etc/hosts', 'utf8');
  })
`;

// A THIRD tenant behaviour -- cleat#2628, the bilateral half of the timeout
// this file already carries. Adding the `timeout` above (this file's own
// prior review round) fixed the omission by hand-verifying it against a
// scratch script; this source string is what makes that verification a
// SHIPPED test instead, run through isolated-wedge.test.ts the same way the
// other two behaviours are. Deliberately a different SHAPE of adversarial
// input from READ_HOST_FILE_SOURCE: that one tries to escape the isolate
// through a host API (`require`), which is refused before it ever runs,
// because a fresh isolate has no such binding. This one never tries to
// escape at all -- it is a tenant script that simply never returns, and the
// isolate's own `require`-less sandboxing does nothing to stop it. Only the
// timeout can.
const INFINITE_LOOP_SOURCE = `
  (function () {
    while (true) {}
  })
`;

// A competent team sandboxing untrusted code bounds its CPU time, not only
// its memory -- cleat-review's review of the first version of this file
// caught the omission: without a `timeout`, a tenant step containing
// `while(true){}` hangs the isolate, the DBOS step, and the workflow
// forever, which is a finding about THIS counterpart being unfinished, not
// about DBOS or isolated-vm. cleat's engine bounds guest execution the same
// way (`tenant_settings.wasm_wall_clock_ceiling_ms`, plus engine-level
// limits) -- an isolated-vm counterpart with no equivalent would be a
// strawman that makes the sandboxed side look weaker than any real team
// would actually ship. 5000ms is generous for either tenant behaviour here
// (both return in well under a second); it exists to bound a runaway
// tenant script, not to constrain a legitimate one.
const TENANT_STEP_TIMEOUT_MS = 5000;

async function runInIsolate(source: string, arg?: string): Promise<string> {
  // memoryLimit is required by isolated-vm and doubles as a real resource
  // bound -- unlike the bare-step version, which has none. Not tuned here;
  // 32 MB is isolated-vm's own example default.
  const isolate = new ivm.Isolate({ memoryLimit: 32 });
  try {
    const context = await isolate.createContext();
    // The timeout applies to EVAL too, not only the call below -- a tenant
    // could just as easily hang the isolate with a top-level infinite loop
    // in the source string itself, before any function is ever invoked.
    const fn = await context.eval(source, { reference: true, timeout: TENANT_STEP_TIMEOUT_MS });
    const args = arg === undefined ? [] : [arg];
    // result: { copy: true } marshals the isolate's return value back into
    // this process as a plain value; without it a Reference leaks the
    // isolate's own memory into the host, which defeats the isolation this
    // file exists to demonstrate.
    const result = await fn.apply(undefined, args, {
      result: { copy: true },
      timeout: TENANT_STEP_TIMEOUT_MS,
    });
    return result as string;
  } finally {
    isolate.dispose();
  }
}

async function normalizeOrderIsolated(input: NormalizeOrderInput): Promise<{
  orderId: string;
  normalized: true;
  transformed: string;
}> {
  const raw = await runInIsolate(
    NORMALIZE_ORDER_SOURCE,
    JSON.stringify({ order_id: input.orderId, vendor_name: input.vendorName }),
  );
  const parsed = JSON.parse(raw);
  return { orderId: parsed.order_id, normalized: true, transformed: parsed.transformed };
}

async function readHostFileIsolated(): Promise<string> {
  return runInIsolate(READ_HOST_FILE_SOURCE);
}

async function infiniteLoopIsolated(): Promise<string> {
  return runInIsolate(INFINITE_LOOP_SOURCE);
}

// runTenantStepIsolated -- same shape as workflow.ts's runTenantStep, same
// DBOS.runStep composition. THE DURABILITY BOUNDARY THIS INTRODUCES IS
// DOCUMENTED IN README.md ("The durability cost this counterpart does not
// remove") RATHER THAN DEMONSTRATED HERE: DBOS.runStep treats the whole
// isolate execution as one opaque unit, and docs.dbos.dev's architecture
// page requires steps to be idempotent because an uncheckpointed step is
// re-executed from the beginning on recovery. Both tenant behaviours below
// are pure functions, so re-execution is harmless FOR THEM -- which is
// exactly why this file cannot exercise the risk: a tenant step with a real
// side effect (charging a card, firing a webhook) would need to be
// idempotent on the TENANT's side, and DBOS has no mechanism -- here or in
// the bare version -- to enforce or verify that for code it does not
// control.
export type TenantStepName = 'normalize-order' | 'read-host-file' | 'infinite-loop';

async function runTenantStepIsolated(stepName: TenantStepName, input: NormalizeOrderInput): Promise<unknown> {
  switch (stepName) {
    case 'normalize-order':
      return DBOS.runStep(() => normalizeOrderIsolated(input), { name: 'normalizeOrderIsolated' });
    case 'read-host-file':
      return DBOS.runStep(() => readHostFileIsolated(), { name: 'readHostFileIsolated' });
    case 'infinite-loop':
      return DBOS.runStep(() => infiniteLoopIsolated(), { name: 'infiniteLoopIsolated' });
  }
}

export const RunTenantStepIsolated = DBOS.registerWorkflow(runTenantStepIsolated, {
  name: 'runTenantStepIsolated',
});
