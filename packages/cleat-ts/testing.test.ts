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

// build.sh names its OUTPUT after the SOURCE file, not the entry -- `out=${OUT_NAME:-$name}` where $name is the source
// basename -- so `build()` above only agrees with it when the entry happens to equal the source name. A file that
// exports two entries needs one build with both, and the module is then the source-named one. Getting this wrong reads
// as "the module does not exist", which is what it looked like the first time.
function buildEntries(relSrc: string, entries: string[]): string {
  execFileSync(join(HERE, "build.sh"), [relSrc, entries.join(",")], { cwd: ROOT, stdio: "pipe" });
  const base = relSrc.slice(relSrc.lastIndexOf("/") + 1).replace(/\.ts$/, "");
  return join(WORK, `${base}.wasm`);
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

// cleat#3828, store support. The pair below is the point of the option: with the outlet scripted the workflow
// COMPLETES, and with the identical fixture and no script it PARKS. One of those alone would not show that the store
// decided anything -- a workflow that ignored its promise would pass the first, and one that failed on any promise
// would look like a passing second.
test("a promise the test scripts is settled, so the workflow finishes", () => {
  const module = build("tests/typescript/testdata/workflows/promises.ts", "promises");
  const run = runWorkflow(module, "promises", {}, {
    promises: { answered: { greeting: "pong" } },
    promiseRejects: { refused: "not today" },
  });
  assert.equal(run.ok, true, `the workflow failed despite a scripted promise: ${run.error}`);
  assert.equal(run.suspended, false, "the workflow parked although every promise it waits on was settled");

  const result = run.result as Record<string, unknown>;
  assert.deepEqual(result.value, { greeting: "pong" }, "the awaited value is not the scripted one");
  assert.match(String(result.rejection), /not today/, "the scripted rejection did not reach the workflow");
  // A promise the workflow settles ITSELF still works with a store present, which is the difference between
  // "the store settles things" and "the store intercepts things".
  assert.equal(result.mineValue, 7);

  // Asserted on the STORE's record, not the workflow's output: the workflow's own return value cannot show that a
  // promise was created.
  assert.deepEqual(run.promises, ["answered", "refused", "pending"]);
});

test("with no script the same workflow PARKS, which is what makes the case above evidence", () => {
  const module = build("tests/typescript/testdata/workflows/promises.ts", "promises");
  const run = runWorkflow(module, "promises", {});
  assert.equal(run.ok, true, `an unscripted promise made the run fail rather than park: ${run.error}`);
  assert.equal(run.suspended, true, "an unscripted promise was settled by something; the store invented an answer");
});

// The signal half. Scripted, the wait is answered on the first poll; unscripted, the same wait parks. DeliveredAtMs is
// left zero by the store deliberately: the engine treats a zero as "a double did not populate it" and answers that the
// delivery is visible, which is the right reading for a signal queued before the run.
test("a signal the test queues before the run is seen by waitForSignal", () => {
  const module = build("tests/typescript/testdata/workflows/signal_store.ts", "signal_store");
  const run = runWorkflow(module, "signal_store", {}, { signals: { approve: [{ ok: true }] } });
  assert.equal(run.ok, true, `the workflow failed despite a queued signal: ${run.error}`);
  assert.equal(run.suspended, false, "the workflow parked although the signal was already queued");
  const result = run.result as Record<string, unknown>;
  assert.deepEqual(result.payload, { ok: true });
  assert.equal(result.timedOut, false);
  assert.deepEqual(run.signalsConsumed, ["approve"], "the store did not record the signal as consumed");
});

test("with no queued signal the same workflow PARKS", () => {
  const module = build("tests/typescript/testdata/workflows/signal_store.ts", "signal_store");
  const run = runWorkflow(module, "signal_store", {});
  assert.equal(run.ok, true, `an unqueued signal made the run fail rather than park: ${run.error}`);
  assert.equal(run.suspended, true, "a signal appeared from nowhere; the store invented a delivery");
});

// cleat#3827, group B. What the workflow SENT is in `extras.signalsSent`, read from the store after the run. A send to another
// run is recorded and is NOT readable back by this run (the harness has no other run), while a send to the run's own id is;
// the second half is what a store that queued every send would get wrong without anything failing.
const SIGNAL_ENTRIES = ["signals_poll", "signals_poll_then_wait", "signals_send", "signals_late"];

test("signalWorkflow is recorded in extras.signalsSent, and only a send to the run's own id comes back to it", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/signals.ts", SIGNAL_ENTRIES);
  const run = runWorkflow(module, "signals_send", { target: "run-elsewhere" });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  const sent = run.extras?.signalsSent as { target: string; name: string; payload: string }[] | undefined;
  assert.ok(sent, `the envelope carries no extras.signalsSent: ${JSON.stringify(run.extras)}`);
  assert.deepEqual(
    sent.map((s) => [s.target, s.name, s.payload]),
    [
      ["run-elsewhere", "hello", '{"n":1}'],
      ["run-elsewhere", "bare", "null"],
      ["00000000-0000-0000-0000-0000000c1ea7", "to-me", '"text"'],
    ],
  );
  // `other` is absent: the send to ANOTHER run was polled back by this one only if the store queued every send.
  assert.deepEqual(run.result, { me: "text" }, "the run's own send did not come back through pollSignal, or a send to another run did");
});

test("a poll sees a signal the test queued and nothing for a name nobody sent", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/signals.ts", SIGNAL_ENTRIES);
  const run = runWorkflow(module, "signals_poll", {}, { signals: { go: [{ n: 7 }] } });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  assert.deepEqual(run.result, { first: { n: 7 }, again: { n: 7 } }, "first and again are the same signal; none is absent");
  assert.deepEqual(run.signalsConsumed ?? [], [], "a poll consumed the signal");
  assert.equal(run.extras?.signalsSent, undefined, "a run that sent nothing reported sends");
});

// Child workflows. Both outcomes are scripted, because a harness that could only say "the child succeeded" would
// leave the parent's error path untested by construction -- and the parent's handling of a rejection is the thing a
// test author actually wants to pin.
test("a child the test scripts returns its result to the parent", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/child_store.ts", ["child_store", "child_store_fails"]);
  const run = runWorkflow(module, "child_store", {}, { children: { charge: { result: { id: "ch_1" } } } });
  assert.equal(run.ok, true, `the workflow failed despite a scripted child: ${run.error}`);
  assert.equal(run.suspended, false, "the workflow parked although its child was scripted as finished");
  assert.deepEqual((run.result as Record<string, unknown>).charged, { id: "ch_1" });
  // From the store, not the workflow's own output: the result alone cannot show that a child was started.
  assert.deepEqual(run.childrenStarted, ["charge"]);
});

test("a child the test scripts as FAILED reaches the parent's error path", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/child_store.ts", ["child_store", "child_store_fails"]);
  const run = runWorkflow(module, "child_store_fails", {}, { children: { charge: { error: "card declined" } } });
  assert.equal(run.ok, true, `the workflow failed instead of catching the child's error: ${run.error}`);
  assert.equal(run.suspended, false);
  assert.match(String((run.result as Record<string, unknown>).failure), /card declined/);
});

test("with no script the same child call PARKS", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/child_store.ts", ["child_store", "child_store_fails"]);
  const run = runWorkflow(module, "child_store", {});
  assert.equal(run.ok, true, `an unscripted child made the run fail rather than park: ${run.error}`);
  assert.equal(run.suspended, true, "an unscripted child was reported finished; the store invented an outcome");
});

// cleat#3827, group E: the lock store. WITHOUT a `locks` script the engine has no ConcurrencyKeyStore and answers EVERY acquire
// "not acquired" while recording the events as if a store had been asked, so a workflow that takes a lock runs its did-not-get-it
// branch and passes having exercised nothing. The second test pins that trap; the first is the store doing its job.
const LOCK_ENTRIES = ["locks_round_trip", "locks_holder", "locks_contender", "locks_expiry", "locks_bad_ttl", "locks_late"];

test("with a locks script, acquire and release behave as a store would, and extras.locks says what it saw", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/locks.ts", LOCK_ENTRIES);
  const run = runWorkflow(module, "locks_round_trip", { key: "mine", other: "theirs" }, {
    scripts: { locks: { held: [{ key: "theirs", holder: "some-other-run" }] } },
  });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  assert.deepEqual(run.result, { first: true, again: false, afterRelease: true, heldElsewhere: false, stillHeldElsewhere: true });
  const l = run.extras?.locks as { acquires: { key: string; ttlMs: number; acquired: boolean }[]; releases: { key: string; released: boolean }[]; held: { key: string; holder: string }[] } | undefined;
  assert.ok(l, `the envelope carries no extras.locks: ${JSON.stringify(run.extras)}`);
  assert.deepEqual(l.acquires.map((a) => [a.key, a.ttlMs, a.acquired]), [
    ["mine", 60000, true], ["mine", 60000, false], ["mine", 60000, true], ["theirs", 60000, false], ["theirs", 60000, false],
  ]);
  // The release of a key this run never held freed nothing, and the key this run took last is still held by it.
  assert.deepEqual(l.releases.map((r) => [r.key, r.released]), [["mine", true], ["theirs", false]]);
  assert.deepEqual(l.held.map((h) => h.key), ["mine", "theirs"]);
  assert.equal(l.held.find((h) => h.key === "theirs")?.holder, "some-other-run", "a release by another run freed the key");
});

test("with NO locks script every acquire is answered not-acquired, which is the engine's behaviour and not the workflow's", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/locks.ts", LOCK_ENTRIES);
  const run = runWorkflow(module, "locks_round_trip", { key: "mine" });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  assert.deepEqual(run.result, { first: false, again: false, afterRelease: false }, "an unscripted run acquired a lock from nowhere");
  assert.equal(run.extras?.locks, undefined, "a store nobody scripted reported something");
});

// cleat#3827 F1: the generic script path. No store group has registered yet, so the only assertion that
// means anything today is the REFUSAL -- and it is the one that matters, because a typo in a store name
// that was silently ignored would configure nothing and leave the run looking fine.
test("a -script name that no store registered is refused, not ignored", () => {
  const module = build("tests/typescript/testdata/workflows/hello.ts", "hello");
  assert.throws(
    () => runWorkflow(module, "hello", {}, { scripts: { nosuchstore: { any: "value" } } }),
    /not a registered store/,
    "an unregistered -script name was accepted; the run would proceed with that store absent",
  );
});

test("a deliberately broken workflow FAILS through the harness", () => {
  const module = build("tests/typescript/testdata/workflows/broken.ts", "broken");
  const run = runWorkflow(module, "broken");
  assert.equal(run.ok, false, "a workflow that throws was reported as successful; the harness cannot detect a failure");
  assert.match(run.error ?? "", /broken on purpose/);
});

// cleat#3827, group A: the update store, driven through the command. This is the test that runs `cleat-ts-run` and reads
// `extras`, which the registry's own unit tests cannot: `extrasFrom` is called after Execute, and moving that call above it
// leaves every unit test green while this one reports the state the run BEGAN with (pending 3, completed none).
const UPDATE_ENTRIES = ["updates", "updates_suspending", "updates_no_handler", "updates_explicit", "updates_late"];

test("an update the test queues is delivered to the workflow's handler, and the store reports what it saw", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/updates.ts", UPDATE_ENTRIES);
  const run = runWorkflow(module, "updates", {}, {
    scripts: { updates: [{ name: "add", payload: { n: 5 } }, { name: "add", payload: { n: "x" } }, { name: "nope" }] },
  });
  assert.equal(run.ok, true, `the workflow failed: ${run.error}`);
  assert.equal(run.suspended, true, "the workflow parks at its sleep, after the dispatch point");
  const u = run.extras?.updates as { completed: { name: string; result?: string; error?: string }[]; pending: number } | undefined;
  assert.ok(u, `the envelope carries no extras.updates: ${JSON.stringify(run.extras)}`);
  assert.equal(u.pending, 0, "a request was still pending: it was reported from the state the run began with, or never delivered");
  assert.deepEqual(
    u.completed.map((c) => [c.name, c.result ?? "", c.error ?? ""]),
    [
      ["add", '{"total":5}', ""],
      ["add", "", "n must be a number"],
      ["nope", "", 'cleat: no update handler registered for "nope"'],
    ],
  );
});

test("with no update scripted the same workflow receives none, and no update store is reported", () => {
  const module = buildEntries("tests/typescript/testdata/workflows/updates.ts", UPDATE_ENTRIES);
  const run = runWorkflow(module, "updates");
  assert.equal(run.ok, true);
  assert.equal(run.suspended, true);
  assert.equal(run.extras?.updates, undefined, "a store nobody scripted reported something");
});
