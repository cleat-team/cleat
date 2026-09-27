/**
 * Tests for the guest-side defer registry (IMPROVEMENT-PLAN §3.73).
 *
 * These use `registerDefer` rather than `deferFunc`, deliberately. `deferFunc`
 * calls the host to mint an ID, and the as-pect harness stubs every
 * `@external("env", ...)` import — so a test built on it would be measuring
 * the stub's return value, not the registry. The end-to-end behaviour, with a
 * real host and a real generated wrapper, is covered by
 * `engine/as_workflow_defer_test.go`.
 */
import {
  HostCalls,
  registerDefer,
  runDeferred,
  pendingDeferCount,
  resetDefers,
  setWorkflowSuspended,
  resetWorkflowSuspended,
  isInDeferPhase,
} from "../index";

// Top-level functions, not closures — see the module docs on defer.ts.
let deferLog: string[] = [];

function resetDeferLog(): void {
  deferLog = [];
  resetDefers();
  resetWorkflowSuspended();
}

function recordPayload(h: HostCalls, payload: string): void {
  deferLog.push(payload);
}

function recordAndSuspend(h: HostCalls, payload: string): void {
  deferLog.push(payload);
  setWorkflowSuspended();
}

describe("defer registry", (): void => {
  it("runs nothing when nothing is registered", (): void => {
    resetDeferLog();
    let h = new HostCalls();
    expect<i32>(runDeferred(h)).toBe(0);
    expect<i32>(deferLog.length).toBe(0);
  });

  it("runs bodies in LIFO order", (): void => {
    resetDeferLog();
    registerDefer("d1", recordPayload, "first");
    registerDefer("d2", recordPayload, "second");
    registerDefer("d3", recordPayload, "third");

    let h = new HostCalls();
    expect<i32>(runDeferred(h)).toBe(3);

    // LIFO is not cosmetic: a defer releases what the defer before it
    // acquired, so registration order would unwind the workflow inside-out.
    expect<i32>(deferLog.length).toBe(3);
    expect<string>(deferLog[0]).toBe("third");
    expect<string>(deferLog[1]).toBe("second");
    expect<string>(deferLog[2]).toBe("first");
  });

  it("hands each body its own payload", (): void => {
    resetDeferLog();
    registerDefer("d1", recordPayload, '{"lock":"orders-42"}');
    let h = new HostCalls();
    runDeferred(h);
    expect<string>(deferLog[0]).toBe('{"lock":"orders-42"}');
  });

  it("is idempotent: a second drain runs nothing", (): void => {
    resetDeferLog();
    registerDefer("d1", recordPayload, "once");

    let h = new HostCalls();
    expect<i32>(runDeferred(h)).toBe(1);
    // Cleanup that runs twice releases a lock twice or refunds a charge twice.
    expect<i32>(runDeferred(h)).toBe(0);
    expect<i32>(deferLog.length).toBe(1);
  });

  it("drains the table before running, not after", (): void => {
    resetDeferLog();
    registerDefer("d1", recordPayload, "a");
    registerDefer("d2", recordPayload, "b");
    expect<i32>(pendingDeferCount()).toBe(2);

    let h = new HostCalls();
    runDeferred(h);
    expect<i32>(pendingDeferCount()).toBe(0);
  });

  it("stops draining if a body suspends the workflow", (): void => {
    resetDeferLog();
    registerDefer("d1", recordPayload, "not-reached");
    registerDefer("d2", recordAndSuspend, "suspends");

    let h = new HostCalls();
    // d2 runs first (LIFO) and sets the flag; d1 belongs to a segment that has
    // not finished, so running it now would fire cleanup for work in flight.
    expect<i32>(runDeferred(h)).toBe(1);
    expect<i32>(deferLog.length).toBe(1);
    expect<string>(deferLog[0]).toBe("suspends");
  });

  it("counts what is pending and nothing else", (): void => {
    resetDeferLog();
    expect<i32>(pendingDeferCount()).toBe(0);
    registerDefer("d1", recordPayload, "a");
    expect<i32>(pendingDeferCount()).toBe(1);
    registerDefer("d2", recordPayload, "b");
    expect<i32>(pendingDeferCount()).toBe(2);
    resetDefers();
    expect<i32>(pendingDeferCount()).toBe(0);
  });
});

// IMPROVEMENT-PLAN §3.35 phase 4: the defer-phase flag.
let phaseLog: bool[] = [];

function recordPhase(h: HostCalls, payload: string): void {
  phaseLog.push(isInDeferPhase());
}

describe("defer phase", (): void => {
  it("is true while a body runs and false either side", (): void => {
    resetDeferLog();
    phaseLog = [];

    // Asserted from INSIDE a body. Outside is exactly where the flag is always
    // false, so a test written there would pass against a flag never set.
    expect<bool>(isInDeferPhase()).toBe(false);
    registerDefer("d1", recordPhase, "");

    let h = new HostCalls();
    expect<i32>(runDeferred(h)).toBe(1);

    expect<i32>(phaseLog.length).toBe(1);
    expect<bool>(phaseLog[0]).toBe(true);
    expect<bool>(isInDeferPhase()).toBe(false);
  });

  it("is cleared on the suspension exit too", (): void => {
    resetDeferLog();
    registerDefer("d1", recordAndSuspend, "suspends");

    let h = new HostCalls();
    runDeferred(h);

    // The early return in runDeferred has its own clear. Without it the next
    // segment's first deferFunc would be refused for a workflow that is simply
    // resuming.
    expect<bool>(isInDeferPhase()).toBe(false);
    resetWorkflowSuspended();
  });
});
