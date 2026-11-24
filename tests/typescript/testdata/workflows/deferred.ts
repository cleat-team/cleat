import { workflow } from "@cleat/ts";

// Deferred work (cleat#3826). Two entries: one finishes (its defers must run) and one suspends (its must NOT).
//
// The bodies write query state, because that is what survives the workflow's return and what Execute hands back. Order is
// observable through the LAST WRITER rather than through history: SetQueryState records no event, it updates an
// in-memory map (engine/lifecycle.go), so two bodies writing one key leave only the value of whichever ran last.

export const deferred = workflow(async (ctx) => {
  // Both bodies write the SAME key, so the value that survives says which ran last. Registered first, so under LIFO it
  // runs last and its value wins: "first".
  await ctx.defer(() => {
    ctx.setQueryState("order", "first");
  });
  await ctx.defer(() => {
    ctx.setQueryState("order", "second");
  });
  // Registered last, so it runs FIRST -- and it throws, which must not stop the two above.
  await ctx.defer(() => {
    ctx.setQueryState("threw", "yes");
    throw new Error("this defer body fails on purpose");
  });
  return { ok: true };
});

// Same registration, then a sleep that suspends the segment. A suspended run has not finished, so its defer must not run:
// firing it here would release work a run that is about to continue still needs.
// The ABI entry is `deferred_suspends`; build.sh wraps the CAMELCASE export (cleat#3911).
export const deferredSuspends = workflow(async (ctx) => {
  await ctx.defer(() => {
    ctx.setQueryState("order", "first");
  });
  await ctx.sleep(60000);
  return { resumed: true };
});
