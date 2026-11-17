import { workflow } from "@cleat/ts";

// cleat#3826, the worker-level half: a run that is CANCELLED while suspended, so the worker owes it a defer segment.
//
// The shape is deliberate and each line earns its place:
//   - the defer's call is the only thing the segment is supposed to reach the outside world with;
//   - `before` runs in the ORDINARY segment, which is what makes "the guest still calls out" a fact rather than an
//     assumption -- without it a segment that recorded nothing would be indistinguishable from a guest that broke;
//   - the sleep is where the run stops, so the cancel arrives while it is suspended and the worker replays it;
//   - `after` is NEW work past the recorded frontier. In a defer segment the host refuses it and the guest unwinds on
//     the sentinel, which is what makes this a segment rather than an ordinary replay. If it ever reaches the caller,
//     the stop is gone.
export const deferTerminal = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  await ctx.call("inventory", "before", {});
  await ctx.sleep(600000);
  await ctx.call("inventory", "after", {});
  return { ok: true };
});
