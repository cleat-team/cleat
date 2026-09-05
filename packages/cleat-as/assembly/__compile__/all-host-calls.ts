/**
 * Every method on HostCalls, called once, so the whole surface is compiled.
 *
 * This exists to be COMPILED, not run. `exerciseEveryHostCall` is never
 * invoked: each call would trap without a host, and several of them
 * (continueAsNew, releaseLock) would change a running workflow's fate. The
 * assertion is that `asc` accepts every call -- an as-pect spec is simply the
 * cheapest place in this package that the compiler already reaches.
 *
 * Why it is needed: examples/as-workflow, examples/widget-store-as and the
 * plugin-harness fixture between them called 11 of 66 methods, so passing the
 * AssemblyScript tests meant "an AS workflow builds", not "the AS host-call
 * surface builds". The Go SDK had exactly that hole and four host calls shipped
 * uncompilable through it -- locks, promises and side effects could not be
 * built from any workflow. See IMPROVEMENT-PLAN.md 3.204, 3.206 and 3.209.
 *
 * This is a compile check, not a behaviour check. It says every method exists
 * with the signature a workflow can call; it says nothing about what the method
 * does. 3.303 is the reminder that those differ: 16 of 17 plugin calls were
 * failing while all five language tests passed.
 *
 * When adding a host call, add it here. scripts/sdk-host-call-coverage.py
 * reports the gap and fails when it grows.
 */
import { ChildWorkflowOptions, HostCalls } from "../host-calls";

// Never called. Its body is the point; `asc` type-checks it either way.
function exerciseEveryHostCall(h: HostCalls): void {
  // ---- durable calls ----
  h.cleatCall("svc", "op", "{}");
  h.cleatCallMs("svc", "op", "{}");
  h.cleatCallRetry("svc", "op", "{}");
  h.cleatCallHeartbeat("svc", "op", "{}", 1000);
  h.cleatSend("svc", "op", "{}");
  h.scheduleInvoke("svc", "op", "{}", 1);
  h.scheduleInvokeMs("svc", "op", "{}", 1000);
  h.runDetached("wf", "{}");

  // ---- sleep, identity, logging ----
  h.cleatSleep(1);
  h.cleatSleepMs(1000);
  h.now();
  h.random();
  h.log("m");
  h.version();
  h.minVersion();
  h.uuid("seed");
  h.currentWorkflowId();
  h.currentRunId();

  // ---- scope ----
  h.setScope("obj", "key");
  h.getScope();
  h.clearScope();

  // ---- defer, lifecycle ----
  h.defer("cleanup");
  h.continueAsNew("{}");
  h.continueAsNewVersioned("{}", 2);
  h.sideEffect("computed");

  // ---- signals ----
  h.pollCancellation();
  h.pollSignal("s");
  h.awaitSignals('["s"]', 1);
  h.awaitSignalsMs('["s"]', 1000);
  h.awaitSignalsWithQuorum('["s"]', 1, 0, 1);
  h.awaitSignalsWithQuorumMs('["s"]', 1, 0, 1000);
  h.signalWorkflow("run-1", "s", "{}");
  h.sendSignalAndWait("run-1", "s", "{}", 1);
  h.sendSignalAndWaitMs("run-1", "s", "{}", 1000);
  h.replyToSignal("corr", "{}");

  // ---- children ----
  h.childWorkflow("child", "{}");
  h.childWorkflowWithOptions("child", "{}", new ChildWorkflowOptions());
  h.awaitChild("run-1");
  h.pollChild("run-1");
  h.awaitAnyChild('["run-1"]');
  h.awaitAllChildren('["run-1"]');

  // ---- promises ----
  h.createPromise("p");
  h.awaitPromise("prom-1", 1);
  h.awaitPromiseMs("prom-1", 1000);
  h.resolvePromise("prom-1", "v");
  h.rejectPromise("prom-1", "e");

  // ---- state ----
  h.setQueryState("k", "v");
  h.setState("k", "v");
  h.getState("k");
  h.deleteState("k");
  h.incrState("k", 1);
  h.hasState("k");
  h.listState("pre");

  // ---- locks ----
  h.acquireLock("k", 1);
  h.acquireLockMs("k", 1000);
  h.releaseLock("k");

  // ---- crons ----
  h.scheduleCron("wf", "* * * * *", "UTC", "{}");
  h.deleteCron("sched-1");
  h.listCrons();

  // ---- http ----
  h.cleatFetch("GET", "http://x", "{}", "");
  h.fetchGet("http://x");

  // ---- plugins ----
  h.pluginCall("p", "f", "{}");
  h.pluginCallStreaming("p", "f", "{}");

  // ---- updates, json helpers ----
  h.registerUpdateHandler("upd");
  h.jsonParse("{}");
  h.jsonStringify('"v"');
}

// No test harness here on purpose. as-pect INSTANTIATES the module it compiles,
// and the host imports are not callable in that runner -- the other specs in
// this package say so, testing "pure functions and constants that do not
// require @external host function imports". Compiling this file is the whole
// assertion, so it is type-checked directly by scripts/check-as-host-calls.sh
// with `asc --noEmit` rather than run.
//
// Referenced so `asc` cannot drop it as unreachable.
export function _keepExerciseEveryHostCallReachable(h: HostCalls): void {
  exerciseEveryHostCall(h);
}
