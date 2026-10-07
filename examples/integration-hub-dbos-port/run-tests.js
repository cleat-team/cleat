// Runs both DBOS counterparts -- the bare-step version (dist/wedge.test.js)
// and the isolated-vm version (dist/isolated-wedge.test.js) -- and combines
// their exit codes into one, because npm's "test" script can only report a
// single status and scripts/run-integration-hub-dbos-scenario.sh translates
// exactly one of these three codes (see that script's header).
//
// COMBINING RULE: report the MORE SEVERE code, where severity is 2 > 1 > 0.
// A 2 (UNMEASURED) from either run means at least one of the two claims this
// pair makes was never actually checked, which is worse than a clean 1
// (a checked claim that came back false) -- so 2 must win over 1, and either
// must win over 0. Running both to completion regardless of the first one's
// result, rather than stopping early, is deliberate: a reader fixing the
// first failure wants to know in the same run whether the second one also
// needs attention.
const { spawnSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');

// CLEAT_STUCK_LOOP_SENTINEL -- cleat#2628, cleat-review's GAP on the first
// version of this mechanism: isolated-wedge.test.ts's runaway-loop test
// self-SIGKILLs when a stuck isolate outlives its own safety margin, and
// `status === null` (killed by signal) used to map unconditionally to 2
// (UNMEASURED) below. That reported the EXACT regression the test exists to
// catch -- the timeout not firing at all -- as "could not measure", which
// contradicted this file's own severity ordering (a checked claim that came
// back false, severity 1, is a less severe report than "nothing was
// checked", severity 2) and the child's own SAFETY_MARGIN_MS comment, which
// says the margin exists so the case is "reported as the FINDING it is".
//
// A sentinel FILE, not just the signal, is what lets this distinguish that
// case from an unrelated SIGKILL (an OOM kill, a CI cancellation) that this
// process had no part in and knows nothing about -- those must still report
// 2, because nothing here measured anything. The child writes '1' to the
// path this passes it, immediately before it kills itself; this reads that
// file only when the child died by signal, and only ever upgrades 2 to 1,
// never invents a false 1.
function run(script) {
  const sentinelPath = path.join(os.tmpdir(), `cleat-stuck-loop-sentinel-${crypto.randomUUID()}`);
  const result = spawnSync(process.execPath, [script], {
    stdio: 'inherit',
    env: { ...process.env, CLEAT_STUCK_LOOP_SENTINEL: sentinelPath },
  });
  if (result.error) {
    console.error(`UNMEASURED: could not run ${script}:`, result.error);
    return 2;
  }
  if (result.status !== null) {
    return result.status;
  }
  let sentinel = '';
  try {
    sentinel = fs.readFileSync(sentinelPath, 'utf8').trim();
  } catch {
    // No sentinel file: either the child never wrote one (an unrelated
    // kill), or it never got the chance to. Either way, 2 is correct.
  }
  try {
    fs.unlinkSync(sentinelPath);
  } catch {
    // Already absent -- fine, nothing to clean up.
  }
  return sentinel === '1' ? 1 : 2;
}

const bareCode = run('dist/wedge.test.js');
const isolatedCode = run('dist/isolated-wedge.test.js');

const combined = Math.max(bareCode, isolatedCode);
console.log(
  `\ncombined result: bare-step=${bareCode} isolated-vm=${isolatedCode} -> exit ${combined}`,
);
process.exit(combined);
