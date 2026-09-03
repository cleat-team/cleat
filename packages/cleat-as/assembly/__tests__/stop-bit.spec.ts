/**
 * The guest half of the defer-segment stop sentinel (IMPROVEMENT-PLAN §3.84,
 * §3.106).
 *
 * When a workflow is replayed as a defer segment — its terminal outcome already
 * decided, the replay existing only to run its outstanding defers — the host
 * refuses any call that would start new work and sets bit 31 of the result word
 * instead. Until this existed the AssemblyScript SDK read that word through
 * whichever layout the call it made returns, and every one of those readings is
 * a plausible ordinary result.
 *
 * These test `stopRequested` and the layout overlap directly. They cannot test
 * the HostCalls methods, because the as-pect harness stubs every
 * `@external("env", ...)` import — a test built on those would be measuring the
 * stub's return value. The structural guarantee that every method calls
 * `stopRequested` before decoding is held from the other side, by
 * `engine/as_sdk_stop_bit_parity_test.go`.
 */
import {
  SUSPEND_STOP_BIT,
  SUSPEND_SENTINEL,
  stopRequested,
  isWorkflowSuspended,
  resetWorkflowSuspended,
  decodeAwaitSignalsResult,
  decodeCallResult,
} from "../index";

describe("the defer-segment stop bit", () => {
  it("reports a refusal and sets the suspension flag", () => {
    resetWorkflowSuspended();
    expect<bool>(stopRequested(SUSPEND_STOP_BIT)).toBe(true);
    expect<bool>(isWorkflowSuspended()).toBe(true);
  });

  it("leaves an ordinary success alone", () => {
    resetWorkflowSuspended();
    // A successful cleat_call: responseLen=1024 in bits 40-63, errCode=0.
    const ok: i64 = 1024 << 40;
    expect<bool>(stopRequested(ok)).toBe(false);
    expect<bool>(isWorkflowSuspended()).toBe(false);
    expect<i32>(decodeCallResult(ok).responseLen as i32).toBe(1024);
  });

  it("leaves an ordinary failure alone", () => {
    // errCode=1 with a message in the buffer is a normal failure, not a stop.
    // A guard that fired on any non-zero word would break every error path.
    resetWorkflowSuspended();
    const err: i64 = (12 << 40) | 1;
    expect<bool>(stopRequested(err)).toBe(false);
    expect<bool>(isWorkflowSuspended()).toBe(false);
  });

  it("is not the export suspend sentinel", () => {
    // The two travel in opposite directions and confusing them is silent:
    // SUSPEND_SENTINEL is bit 62, what the guest returns to the host from an
    // export, and the host never sets it in a result word.
    resetWorkflowSuspended();
    expect<i64>(SUSPEND_STOP_BIT).toBe(0x80000000);
    expect<i64>(SUSPEND_SENTINEL).toBe(0x4000000000000000);
    expect<i64>(SUSPEND_STOP_BIT & SUSPEND_SENTINEL).toBe(0);
    expect<bool>(stopRequested(SUSPEND_SENTINEL)).toBe(false);
  });

  it("would be read as an ordinary timeout if the fields were decoded first", () => {
    // This is why stopRequested must be called BEFORE any field is decoded,
    // stated as a test rather than as a comment. In the await-signals layout
    // bit 31 lands inside the timed-out field, so a caller that decoded first
    // would return a normal timeout and the workflow would carry on — doing
    // the new work the defer segment exists to prevent, with nothing to see.
    //
    // If this ever fails because the layout moved, the ordering requirement has
    // not gone away; it has moved to whichever field now overlaps bit 31.
    expect<bool>(decodeAwaitSignalsResult(SUSPEND_STOP_BIT).timedOut).toBe(true);
  });
});
