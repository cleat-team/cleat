import { workflow } from "@cleat/ts";

// cleat#3827, group A. Registers handlers, then reaches a suspension point (`sleep`), where the pending updates are
// delivered. The sleep suspends the segment, so everything observable is in the store and the history, not in a result.
export const updates = workflow<Record<string, never>, { total: number }>(async (ctx) => {
  let total = 0;
  await ctx.registerUpdateHandler<{ n: number }, { total: number }>(
    "add",
    (input) => {
      total += input.n;
      return { total };
    },
    // The validator is read-only and runs first: a throw refuses the update without running the handler.
    (input) => {
      if (typeof input.n !== "number") throw new Error("n must be a number");
    },
  );
  await ctx.registerUpdateHandler("boom", () => {
    throw new Error("boom");
  });
  await ctx.registerUpdateHandler("quiet", () => {
    /* returns nothing */
  });
  await ctx.sleep(60_000);
  return { total };
});

// A handler that suspends in the middle of its own body: the delivery is recorded, the completion is not, and the request
// stays pending, so it is redelivered on the next segment.
export const updatesSuspending = workflow<Record<string, never>, null>(async (ctx) => {
  await ctx.registerUpdateHandler("slow", async () => {
    await ctx.sleep(60_000);
    return "done";
  });
  await ctx.sleep(120_000);
  return null;
});

// No handler registered at all: a delivered update is still answered, with an error, as the Rust SDK does.
export const updatesNoHandler = workflow<Record<string, never>, null>(async (ctx) => {
  await ctx.sleep(60_000);
  return null;
});

// An explicit dispatch point, with no suspension before it: the update is handled where the workflow says.
export const updatesExplicit = workflow<Record<string, never>, { total: number }>(async (ctx) => {
  let total = 0;
  await ctx.registerUpdateHandler<{ n: number }, number>("add", (input) => (total += input.n));
  await ctx.dispatchUpdates();
  return { total };
});

// Registers a handler AFTER the call that suspends the segment, and never awaits it. A segment records nothing past the call
// that suspended it, so the registration must not reach the host: with the binding unguarded, an update_handler event is
// recorded for a registration the run never reached. See TestTypeScriptARegistrationAfterASuspensionRecordsNothing.
export const updatesLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.registerUpdateHandler("late", () => 1);
  await p;
  return null;
});
