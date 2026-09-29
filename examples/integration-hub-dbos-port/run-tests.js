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

function run(script) {
  const result = spawnSync(process.execPath, [script], { stdio: 'inherit' });
  if (result.error) {
    console.error(`UNMEASURED: could not run ${script}:`, result.error);
    return 2;
  }
  return result.status === null ? 2 : result.status;
}

const bareCode = run('dist/wedge.test.js');
const isolatedCode = run('dist/isolated-wedge.test.js');

const combined = Math.max(bareCode, isolatedCode);
console.log(
  `\ncombined result: bare-step=${bareCode} isolated-vm=${isolatedCode} -> exit ${combined}`,
);
process.exit(combined);
