// The harness's own test (cleat#3828). Both directions, in one file, for the reason #3927's defer test gives: an
// assertion that something works passes trivially if the thing never runs at all. So one test runs an example's compiled
// module and asserts what it returned, and another runs a DELIBERATELY BROKEN workflow and asserts that it fails.
//
// Every module here is built by packages/cleat-ts/build.sh first: the harness's whole claim is that it runs the deployed
// artefact, so a fixture compiled any other way would not be testing that claim.

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { runWorkflow } from "./testing.ts";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "../..");
const WORK = join(HERE, ".work");

function build(relSrc: string, entry: string): string {
  execFileSync(join(HERE, "build.sh"), [relSrc, entry], { cwd: ROOT, stdio: "pipe" });
  return join(WORK, `${entry}.wasm`);
}

// Built once for the whole file; `go build` and build.sh are both cached after the first call.
process.env.CLEAT_TS_RUN ??= (() => {
  const bin = join(WORK, "cleat-ts-run");
  execFileSync("go", ["build", "-o", bin, "./cmd/cleat-ts-run"], { cwd: ROOT, stdio: "pipe" });
  return bin;
})();

test("an example workflow runs through the harness and returns its result", () => {
  const module = build("tests/typescript/testdata/workflows/hello.ts", "hello");
  const run = runWorkflow(module, "hello");
  assert.equal(run.ok, true, `the example workflow failed: ${run.error}`);
  assert.equal(run.suspended, false);
  assert.deepEqual(run.result, { hello: "ts" });
});

// cleat#3828: the scripted-answer path, both directions. The first test would pass with NO scripting at all if the
// workflow ignored its answer, which is why the fixture returns the value instead of discarding it; the second pins
// that a forgotten stub is a REFUSAL naming the key rather than a silent `{}`.
test("a durable call is answered from the test's script", () => {
  const module = build("tests/typescript/testdata/workflows/answer.ts", "answer");
  const run = runWorkflow(module, "answer", {}, { answers: { "payments/charge": JSON.stringify({ id: "ch_1" }) } });
  assert.equal(run.ok, true, `the workflow failed despite a scripted answer: ${run.error}`);
  assert.deepEqual(run.result, { charged: { id: "ch_1" } });
  assert.equal(run.stubbedCalls, undefined, "the envelope says the calls were stubbed, but a script was supplied");
});

test("a call the SUPPLIED script does not name is refused, naming the key", () => {
  // A script must be supplied for this state to exist at all: with no -answers the harness stubs every call with `{}`
  // and the workflow succeeds. My first version of this test passed NO options and asserted both `ok: false` and
  // `stubbedCalls: true`, which cannot both hold -- cleat-review caught it in CI, where the positive half passed.
  const module = build("tests/typescript/testdata/workflows/answer.ts", "answer");
  const run = runWorkflow(module, "answer", {}, { answers: { "inventory/reserve": JSON.stringify({ ok: true }) } });
  assert.equal(run.ok, false, "a call the script omitted was answered instead of refused");
  assert.match(run.error ?? "", /payments\/charge/);
  assert.equal(run.stubbedCalls, undefined, "a script was supplied, so the run is not a stubbed one");
});

test("with no script at all every call is stubbed, and the envelope says so", () => {
  // The documented default, pinned: the workflow passes because the stub answers `{}`, and `stubbedCalls` is what stops
  // that green being read as "the calls were tested".
  const module = build("tests/typescript/testdata/workflows/answer.ts", "answer");
  const run = runWorkflow(module, "answer", {});
  assert.equal(run.ok, true, `the stubbed run failed: ${run.error}`);
  assert.equal(run.stubbedCalls, true);
});

test("a workflow that suspends is reported as suspended, with the host's reason", () => {
  const module = build("tests/typescript/testdata/workflows/sleeper.ts", "sleeper");
  const run = runWorkflow(module, "sleeper", { ms: 60000 });
  assert.equal(run.ok, true);
  assert.equal(run.suspended, true);
  assert.match(run.suspendReason ?? "", /cleat_sleep\(60000ms\)/);
});

test("a deliberately broken workflow FAILS through the harness", () => {
  const module = build("tests/typescript/testdata/workflows/broken.ts", "broken");
  const run = runWorkflow(module, "broken");
  assert.equal(run.ok, false, "a workflow that throws was reported as successful; the harness cannot detect a failure");
  assert.match(run.error ?? "", /broken on purpose/);
});
