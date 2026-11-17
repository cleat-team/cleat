import { workflow } from "@cleat/ts";

// cleat#3826, the worker-level half: a run that is CANCELLED while suspended, so the worker owes it a defer segment.
//
// The shape is deliberate and each line earns its place:
//   - the defer's call is the only thing the segment is supposed to reach the outside world with;
//   - `before` runs in the ORDINARY segment, which is what makes "the guest still calls out" a fact rather than an
//     assumption -- without it a segment that recorded nothing would be indistinguishable from a guest that broke;
//   - the SHORT sleep then `resumed` is the observable RESUME the issue's done-when asks for: the run stops, wakes on
//     the timer, and carries on. Without it the test would show a workflow that suspends and is cancelled, which is not
//     the same claim;
//   - the long sleep is where the cancel arrives, so the worker owes the run a defer phase;
//   - `after` is NEW work past the recorded frontier. In a defer segment the host refuses it and the guest unwinds on
//     the sentinel, which is what makes this a segment rather than an ordinary replay. If it ever reaches the caller,
//     the stop is gone.
export const deferTerminal = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  await ctx.call("inventory", "before", {});
  await ctx.sleep(1500);
  await ctx.call("inventory", "resumed", {});
  await ctx.sleep(600000);
  await ctx.call("inventory", "after", {});
  return { ok: true };
});
