// The test for examples/ts-workflow, and the one place the commands in docs/tutorials/typescript-quick-start.md are run.
//
// It builds the module with `cleat build --target ts`, which is the command the page documents, not packages/cleat-ts/build.sh
// directly, so a change that breaks the documented command breaks this. Then it runs the compiled module through the test
// harness, in both directions: the happy path with every call scripted, and a call the script forgot to name, which must be
// REFUSED. The second is what makes the first mean something: with no refusal, a harness that answered every call `{}` would
// pass a workflow that ignored what it was told.
//
// Run: node --test examples/ts-workflow/workflow.test.ts   (from the repository root, after packages/cleat-ts/bootstrap.sh)

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { runWorkflow } from "../../packages/cleat-ts/testing.ts";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, "../..");
const WORK = join(ROOT, "packages", "cleat-ts", ".work");
mkdirSync(WORK, { recursive: true });

process.env.CLEAT_TS_RUN ??= (() => {
  const bin = join(WORK, "cleat-ts-run");
  execFileSync("go", ["build", "-o", bin, "./cmd/cleat-ts-run"], { cwd: ROOT, stdio: "pipe" });
  return bin;
})();

// The documented build, from the repository root, into a fresh directory. Exactly one .wasm must come out.
function documentedBuild(): string {
  const cleat = join(WORK, "cleat");
  execFileSync("go", ["build", "-o", cleat, "./cmd/cleat"], { cwd: ROOT, stdio: "pipe" });
  const out = mkdtempSync(join(tmpdir(), "ts-workflow-"));
  execFileSync(cleat, ["build", "--target", "ts", "-o", out, "examples/ts-workflow/workflow.ts:greet"], { cwd: ROOT, stdio: "pipe" });
  const wasm = readdirSync(out).filter((f) => f.endsWith(".wasm"));
  assert.equal(wasm.length, 1, `the documented build wrote ${wasm.length} .wasm files (${wasm.join(", ")}), want exactly 1`);
  return join(out, wasm[0]);
}

const module = documentedBuild();

test("the documented build produces a module whose result depends on the scripted answers", () => {
  const run = runWorkflow(module, "greet", { name: "Ada" }, {
    answers: {
      "greeter/compose": JSON.stringify({ text: "Hello, Ada" }),
      "audit/record": JSON.stringify({ id: "a-1" }),
    },
  });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  assert.equal(run.suspended, false);
  assert.deepEqual(run.result, { greeting: "Hello, Ada", auditId: "a-1" });
});

test("a call the script does not name is refused, naming the key", () => {
  const run = runWorkflow(module, "greet", { name: "Ada" }, { answers: { "greeter/compose": JSON.stringify({ text: "Hello, Ada" }) } });
  assert.equal(run.ok, false, "the workflow succeeded although audit/record was never scripted");
  assert.match(String(run.error), /audit\/record/, `the refusal does not name the missing key: ${run.error}`);
});
