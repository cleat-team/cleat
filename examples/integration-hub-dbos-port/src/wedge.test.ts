// Executed assertions against a real DBOS runtime and a real Postgres --
// cleat#2597's second wedge pair. No test framework, matching
// order.test.ts's own convention: run with `npm run build && npm test`
// against DBOS_SYSTEM_DATABASE_URL.
//
// THE CLAIM THIS PAIR CARRIES (criterion 3), read together with the
// order-lifecycle pair's claim rather than in isolation: that pair says the
// COMPARISON is honest; this one says where a real boundary sits. Neither is
// phrased as a win.
//
//   "A DBOS step has no platform-enforced boundary against host filesystem
//    access, so isolating a per-tenant step is entirely the application
//    author's responsibility to build; cleat's WASI policy
//    (engine/wasi_policy.go) provides this boundary as part of the
//    platform, enforced once for every tenant step rather than reinvented
//    per application."
//
// WHY A TRIVIALLY-TRUE RESULT IS EVIDENCE ABOUT DBOS, NOT ABOUT NODE.
// readHostFile() succeeding is unsurprising by itself -- Node has full host
// access by design, and nobody familiar with it expects otherwise. That
// observation is NOT what this test is evidence for. The claim is about
// DBOS's platform SURFACE: DBOS *could* have interposed something between a
// step and the process it runs in -- a wrapped executor, a required
// isolate, a capability check -- the way cleat's engine interposes
// engine/wasi_policy_wasmtime.go between a guest and every WASI import. It
// does not: nothing in DBOS.runStep, DBOS.registerWorkflow, or anything
// else in the SDK's public API touches what a step function is allowed to
// do. So the success below is evidence that the PLATFORM's contribution to
// isolation is zero, not evidence about what Node can do -- the finding is
// the absence of anything between "a DBOS step" and "an ordinary function
// call", and this test is how that absence is measured rather than merely
// asserted in prose.
//
// READING THIS FILE'S EXIT CODES -- NOT THE USUAL SHAPE, READ BEFORE ACTING
// ON A NON-ZERO STATUS:
//
//   0  both assertions held: the positive control ran, and readHostFile
//      succeeded exactly as documented above.
//   1  A FINDING, not "the test failed" in the usual sense: readHostFile
//      was refused or threw. That would FALSIFY the claim this file states
//      -- it means today's DBOS does not behave the way this pair documents
//      -- and needs investigating (did the SDK change? did something
//      outside DBOS, e.g. a container's read-only filesystem, intervene?),
//      never silently accepted or loosened away.
//   2  UNMEASURED: the positive control itself did not succeed, or the
//      harness crashed before reaching a verdict. Neither says anything
//      about the boundary this pair exists to measure -- see README.md,
//      "Reading this pair's exit codes", because `1` conventionally reads
//      as "the test failed" and a future reader will assume a broken run
//      unless told otherwise.
import { DBOS } from '@dbos-inc/dbos-sdk';
import { RunTenantStep } from './workflow';

const CLAIM =
  'integration-hub-dbos-port: a DBOS step has no platform-enforced boundary ' +
  "against host filesystem access -- isolating a tenant's step is entirely " +
  "the application author's to build; cleat's WASI policy provides this " +
  'boundary as part of the platform (cleat#2597).';

let positiveControlHeld = false;
let findingFalsified = false;

function log(ok: boolean, why: string) {
  console.log(`${ok ? 'ok' : 'FAIL'}: ${why}`);
}

// THE MANDATORY POSITIVE CONTROL. If this does not hold, nothing below is
// trustworthy -- a broken harness that fails every step would report
// readHostFile as "refused" for a reason that has nothing to do with DBOS's
// isolation surface.
async function testPositiveControlNormalizeOrderSucceeds() {
  const handle = await DBOS.startWorkflow(RunTenantStep)('normalize-order', {
    orderId: 'ord-dbos-1',
    vendorName: 'acme',
  });
  const result = (await handle.getResult()) as NormalizeOrderResultShape;
  positiveControlHeld = result?.normalized === true;
  log(positiveControlHeld, 'the legitimate tenant step (normalize-order) completed -- the harness itself works');
}

interface NormalizeOrderResultShape {
  normalized: boolean;
}

// THE FINDING: no trap, no refusal. The read succeeds because nothing in
// DBOS's execution path is positioned to refuse it. See this file's header
// comment for why that is evidence about DBOS's surface, not about Node.
async function testReadHostFileSucceedsBecauseNothingStopsIt() {
  const handle = await DBOS.startWorkflow(RunTenantStep)('read-host-file', { orderId: 'unused' });
  try {
    const content = (await handle.getResult()) as string;
    const ok = typeof content === 'string' && content.length > 0;
    if (!ok) {
      findingFalsified = true;
    }
    log(
      ok,
      "readHostFile returned the host's /etc/hosts -- DBOS interposed nothing between the step and the filesystem",
    );
  } catch (e) {
    findingFalsified = true;
    console.error(
      `FALSIFIED: readHostFile was refused (${e}) -- today's DBOS does not do this. ` +
        "Re-check this pair's finding against current docs.dbos.dev before assuming this test is wrong.",
    );
  }
}

async function main() {
  console.log(`CLAIM: ${CLAIM}`);
  DBOS.setConfig({
    name: 'integration-hub-dbos-port-test',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  await DBOS.launch();

  await testPositiveControlNormalizeOrderSucceeds();
  if (positiveControlHeld) {
    await testReadHostFileSucceedsBecauseNothingStopsIt();
  } else {
    console.error('UNMEASURED: the positive control failed, so readHostFile was not evaluated -- ' +
      'this run says nothing about the boundary this pair measures');
  }

  await DBOS.shutdown();

  if (!positiveControlHeld) {
    console.error('UNMEASURED: harness precondition (positive control) not met');
    process.exit(2);
  }
  if (findingFalsified) {
    console.error('FINDING: readHostFile was refused -- see this file\'s header comment before treating this as a bug here');
    process.exit(1);
  }
  console.log('all assertions passed -- the documented finding holds');
}

main().catch((e) => {
  console.error('UNMEASURED: test run crashed before reaching a verdict:', e);
  process.exit(2);
});
