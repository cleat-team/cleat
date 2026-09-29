// Executed assertions for the SECOND DBOS counterpart -- cleat#2597. Read
// wedge.test.ts's header first: that file measures bare DBOS.runStep with no
// sandbox, and this file measures the idiomatic fix a real team would apply
// (isolated-vm). The two are complementary, not competing: wedge.test.ts's
// finding ("DBOS supplies no boundary") is still true and still the reason
// this file's extra machinery exists at all.
//
// THE CLAIM THIS FILE CARRIES, read together with wedge.test.ts's claim
// rather than in isolation: "an idiomatic team CAN build a real boundary
// around DBOS-hosted tenant code using a third-party sandbox library, at a
// measurable line-count cost this file's own size documents -- and DBOS
// itself contributes nothing to that boundary either way; the isolation
// comes entirely from the library, not the platform."
//
// WHY THE VERDICT FLIPS FROM wedge.test.ts, AND WHY THAT IS NOT A
// CONTRADICTION. There, readHostFile succeeds, because nothing stands
// between a DBOS step and the process it runs in. Here, the equivalent read
// is REFUSED -- not by DBOS, by isolated-vm, which starts a fresh V8 isolate
// with no `require`, no `fs`, no ambient Node built-ins at all. Verified
// empirically below, the same way wedge.test.ts verifies its own opposite
// finding: neither file asks the reader to take its claim on faith.
//
// READING THIS FILE'S EXIT CODES -- SAME CONTRACT AS wedge.test.ts, restated
// because the polarity is opposite and a reader skimming only exit codes
// could otherwise misread which file is which:
//
//   0  both assertions held: the positive control ran through the isolate,
//      and the adversarial read was REFUSED by the isolate exactly as
//      documented.
//   1  A FINDING: the adversarial read SUCCEEDED despite running inside the
//      isolate -- meaning isolated-vm's isolation boundary did not hold.
//      That is a serious result (a sandbox library not sandboxing) and
//      needs investigating immediately, not waving through.
//   2  UNMEASURED: the positive control itself did not hold, or the harness
//      crashed before reaching a verdict.
import { DBOS } from '@dbos-inc/dbos-sdk';
import { RunTenantStepIsolated } from './isolated-workflow';

const CLAIM =
  'integration-hub-dbos-port (isolated counterpart): wrapping DBOS.runStep ' +
  'around an isolated-vm isolate gives a tenant step a real boundary against ' +
  "host filesystem access -- the isolation is the LIBRARY's contribution, " +
  'not DBOS\'s, and it costs real application lines to build (cleat#2597).';

let positiveControlHeld = false;
let isolationHeld = false;

function log(ok: boolean, why: string) {
  console.log(`${ok ? 'ok' : 'FAIL'}: ${why}`);
}

interface NormalizeOrderResultShape {
  normalized: boolean;
}

// THE MANDATORY POSITIVE CONTROL -- same requirement as wedge.test.ts's, and
// doubly important here: this run also proves the isolate wiring itself
// works (context creation, source eval, marshalling a result back out),
// before any claim is made about what the isolate refuses.
async function testPositiveControlNormalizeOrderSucceedsThroughIsolate() {
  const handle = await DBOS.startWorkflow(RunTenantStepIsolated)('normalize-order', {
    orderId: 'ord-dbos-isolated-1',
    vendorName: 'acme',
  });
  const result = (await handle.getResult()) as NormalizeOrderResultShape;
  positiveControlHeld = result?.normalized === true;
  log(
    positiveControlHeld,
    'the legitimate tenant step completed through the isolate -- the isolate wiring itself works',
  );
}

// THE FINDING FOR THIS COUNTERPART: the isolate refuses the read.
async function testReadHostFileIsRefusedByTheIsolate() {
  const handle = await DBOS.startWorkflow(RunTenantStepIsolated)('read-host-file', { orderId: 'unused' });
  try {
    const content = await handle.getResult();
    isolationHeld = false;
    console.error(
      `FINDING: the isolate did NOT refuse the read -- got ${JSON.stringify(content)}. ` +
        "isolated-vm's isolation did not hold; investigate immediately.",
    );
  } catch (e) {
    isolationHeld = true;
    log(true, `readHostFile was refused by the isolate (${e}) -- exactly as documented`);
  }
}

async function main() {
  console.log(`CLAIM: ${CLAIM}`);
  DBOS.setConfig({
    name: 'integration-hub-dbos-port-isolated-test',
    systemDatabaseUrl: process.env.DBOS_SYSTEM_DATABASE_URL,
  });
  await DBOS.launch();

  await testPositiveControlNormalizeOrderSucceedsThroughIsolate();
  if (positiveControlHeld) {
    await testReadHostFileIsRefusedByTheIsolate();
  } else {
    console.error(
      'UNMEASURED: the positive control failed, so the isolation test was not evaluated -- ' +
        'this run says nothing about the boundary this counterpart measures',
    );
  }

  await DBOS.shutdown();

  if (!positiveControlHeld) {
    console.error('UNMEASURED: harness precondition (positive control) not met');
    process.exit(2);
  }
  if (!isolationHeld) {
    console.error('FINDING: the isolate failed to refuse the adversarial read -- see this file\'s header comment');
    process.exit(1);
  }
  console.log('all assertions passed -- the isolate boundary holds');
}

main().catch((e) => {
  console.error('UNMEASURED: test run crashed before reaching a verdict:', e);
  process.exit(2);
});
