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
// A THIRD ASSERTION -- cleat#2628, the bilateral half of a suggestion
// cleat-review made reviewing #2621: this file's `timeout` option (added
// after that review caught it was missing) had only ever been verified by a
// scratch script, not a shipped test, and only on this one side of the
// cleat-vs-DBOS pair -- cleat's own equivalent bound
// (tenant_settings.wasm_instance_timeout_ms) is exercised through
// scripts/run-integration-hub-tenant-sandbox-scenario.sh's real deployed
// path, so "bounded execution" was a tested claim on one side and an
// assertion-by-hand on the other. testRunawayLoopIsInterruptedByTheTimeout
// below closes that gap on this side: it asserts the isolate's `timeout`
// actually throws for a script that never returns, through the same
// DBOS.startWorkflow entry point the other two assertions use, not by
// calling runInIsolate directly.
//
// READING THIS FILE'S EXIT CODES -- SAME CONTRACT AS wedge.test.ts, restated
// because the polarity is opposite and a reader skimming only exit codes
// could otherwise misread which file is which:
//
//   0  all three assertions held: the positive control ran through the
//      isolate, the adversarial read was REFUSED by the isolate, and a
//      runaway loop was INTERRUPTED by the isolate's timeout -- all exactly
//      as documented.
//   1  A FINDING: the adversarial read SUCCEEDED despite running inside the
//      isolate, OR the runaway loop was not interrupted -- either it
//      completed with a result, or it hung past this file's own
//      SAFETY_MARGIN_MS with no result and no error at all. Either way,
//      isolated-vm's isolation boundary did not hold. A serious result (a
//      sandbox library not sandboxing) and needs investigating immediately,
//      not waving through.
//
//      THE SECOND CASE IS REPORTED VIA run-tests.js, NOT DIRECTLY: a
//      process with a genuinely wedged native isolate thread cannot be made
//      to exit(1) cleanly (measured -- see SAFETY_MARGIN_MS's comment), only
//      SIGKILLed, and a process killed by signal has no exit code of its
//      own. This file writes a sentinel FILE naming the finding before it
//      kills itself; run-tests.js (the only thing that runs this file
//      directly) reads that file and reports 1, not 2, when it is present.
//      Running this file some OTHER way -- directly, bypassing run-tests.js
//      -- loses that translation and a stuck loop will report as if killed
//      for an unrelated reason. See run-tests.js's own comment for why the
//      sentinel is a file and not just the signal.
//   2  UNMEASURED: the positive control itself did not hold, the harness
//      crashed before reaching a verdict, or (via run-tests.js) this
//      process was killed by a signal it did not send itself -- an OOM
//      kill, a CI cancellation -- which says nothing about the boundary
//      this file measures.
import { DBOS } from '@dbos-inc/dbos-sdk';
import * as fs from 'node:fs';
import { RunTenantStepIsolated } from './isolated-workflow';

const CLAIM =
  'integration-hub-dbos-port (isolated counterpart): wrapping DBOS.runStep ' +
  'around an isolated-vm isolate gives a tenant step a real boundary against ' +
  "host filesystem access -- the isolation is the LIBRARY's contribution, " +
  'not DBOS\'s, and it costs real application lines to build (cleat#2597).';

let positiveControlHeld = false;
let isolationHeld = false;
let timeoutHeld = false;
let stuckLoopStillRunning = false;

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

// THE SECOND FINDING FOR THIS COUNTERPART: the isolate interrupts a runaway
// loop rather than hanging the DBOS step (and, through it, the workflow)
// forever. DBOS.startWorkflow / getResult have no timeout of their own on
// this call, so a test that got this wrong would hang rather than fail --
// worth naming, because it means this test's own correctness depends on the
// isolate's timeout actually firing, the same property it exists to check.
//
// THAT HANG IS NOT HYPOTHETICAL, AND THIS FILE DOES NOT LEAVE IT UNBOUNDED.
// Falsified by removing isolated-workflow.ts's `timeout` from the fn.apply
// call that actually runs the loop (context.eval's own timeout only bounds
// PARSING the source, so it returns immediately regardless): the process
// hung past 15s with no error and no result. `npm test` here feeds
// scripts/run-integration-hub-dbos-scenario.sh, whose CI job
// (integration-hub-dbos-pair-scenario in .github/workflows/ci.yml) sets no
// `timeout-minutes` -- like every job in that file -- so an unbounded wait
// here would not fail the job, it would occupy a GitHub Actions runner for
// up to the platform's own 360-minute default. SAFETY_MARGIN_MS races
// getResult() against a plain setTimeout, independent of and far larger
// than isolated-vm's own bound (whatever that is currently configured to),
// so a regression here is reported as the FINDING it is -- "the loop was
// not interrupted" -- rather than a stalled CI job with no message at all.
//
// GETTING TO "reported as 1" TOOK A SECOND FIX, caught in review rather
// than by running it: the margin firing still has to SIGKILL this process
// (see the stuckLoopStillRunning branch in main(), below), and a
// signal-killed process has no exit code of its own for run-tests.js's
// spawnSync to read -- it mapped to 2 (UNMEASURED) unconditionally, which
// reported the exact regression this margin exists to catch as "nothing was
// checked", the opposite of this paragraph's claim. The sentinel file
// run-tests.js passes via CLEAT_STUCK_LOOP_SENTINEL, written just before the
// kill, is what actually closes the gap between this comment and what
// happened.
const SAFETY_MARGIN_MS = 15_000;

function delay(ms: number): Promise<{ safetyMarginFired: true }> {
  return new Promise((resolve) => setTimeout(() => resolve({ safetyMarginFired: true }), ms));
}

async function testRunawayLoopIsInterruptedByTheTimeout() {
  const handle = await DBOS.startWorkflow(RunTenantStepIsolated)('infinite-loop', { orderId: 'unused' });
  try {
    const outcome = await Promise.race([handle.getResult(), delay(SAFETY_MARGIN_MS)]);
    timeoutHeld = false;
    if (outcome && (outcome as { safetyMarginFired?: true }).safetyMarginFired) {
      // The isolate's OWN timeout never fired, so the underlying step is still
      // genuinely stuck in a native busy loop -- observed directly, once,
      // falsifying this test: DBOS.shutdown() below printed one log line
      // ("Shutting down while N workflows are still running") and then hung
      // itself, indefinitely, rather than returning. Recorded here rather than
      // fixed in DBOS.shutdown(), which this file does not own; main() below
      // skips the graceful shutdown entirely when this flag is set, for
      // exactly that reason.
      stuckLoopStillRunning = true;
      console.error(
        `FINDING: a runaway loop was still running after this test's own ${SAFETY_MARGIN_MS}ms safety ` +
          "margin, well past isolated-vm's own configured timeout -- the isolate's timeout did not fire " +
          'at all; investigate immediately. (This margin exists so a regression here fails this ' +
          'assertion instead of hanging the CI job -- see the comment above.)',
      );
    } else {
      console.error(
        `FINDING: a runaway loop completed with a result (${JSON.stringify(outcome)}) instead of being ` +
          "interrupted -- isolated-vm's timeout did not hold; investigate immediately.",
      );
    }
  } catch (e) {
    timeoutHeld = true;
    log(true, `the runaway loop was interrupted by the isolate's timeout (${e}) -- exactly as documented`);
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
    await testRunawayLoopIsInterruptedByTheTimeout();
  } else {
    console.error(
      'UNMEASURED: the positive control failed, so the isolation tests were not evaluated -- ' +
        'this run says nothing about the boundary this counterpart measures',
    );
  }

  if (stuckLoopStillRunning) {
    // Do NOT await DBOS.shutdown() here, and do NOT use process.exit() either
    // -- both were tried and both failed, in that order, each only found by
    // actually running this branch to completion under an external hard
    // bound (`timeout 40 npm test`), not by reading the code:
    //
    //  1. await DBOS.shutdown() printed one log line ("Shutting down while N
    //     workflows are still running") and then hung indefinitely.
    //  2. Skipping it and calling process.exit(1) directly did NOT terminate
    //     the process either -- confirmed by measurement, `timeout 40` had
    //     to SIGKILL it from outside. isolated-vm's native busy-loop thread
    //     has nothing checking for Node's normal exit signal, so the process
    //     keeps running with the event loop otherwise empty.
    //
    // SIGKILL to itself is what actually works, because the OS terminates
    // the whole process unconditionally -- no JS-level or native-level code
    // gets a chance to keep a thread alive against it, which is exactly the
    // property everything above lacked.
    console.error(
      'a workflow is still genuinely running; DBOS.shutdown() and process.exit() do not ' +
        'terminate a process with a wedged native isolate thread -- sending SIGKILL to self',
    );
    // A SIGKILL alone reports as UNMEASURED to run-tests.js -- cleat-review's
    // GAP on the first version of this fix: `status === null` (killed by
    // signal) mapped unconditionally to 2, which reports the EXACT
    // regression this test exists to catch as "could not measure", the
    // opposite of what SAFETY_MARGIN_MS's own comment says happens. Write
    // the sentinel file run-tests.js gave this process, BEFORE the kill --
    // there is no after. See run-tests.js's own comment for the other half.
    const sentinelPath = process.env.CLEAT_STUCK_LOOP_SENTINEL;
    if (sentinelPath) {
      try {
        fs.writeFileSync(sentinelPath, '1');
      } catch (e) {
        console.error(`could not write the stuck-loop sentinel to ${sentinelPath}: ${e}`);
      }
    }
    process.kill(process.pid, 'SIGKILL');
    return; // unreachable; SIGKILL does not return control to this process
  }
  await DBOS.shutdown();

  if (!positiveControlHeld) {
    console.error('UNMEASURED: harness precondition (positive control) not met');
    process.exit(2);
  }
  if (!isolationHeld || !timeoutHeld) {
    console.error(
      'FINDING: the isolate failed to hold one of its boundaries -- see this file\'s header comment',
    );
    process.exit(1);
  }
  console.log('all assertions passed -- the isolate boundary holds');
}

main().catch((e) => {
  console.error('UNMEASURED: test run crashed before reaching a verdict:', e);
  process.exit(2);
});
