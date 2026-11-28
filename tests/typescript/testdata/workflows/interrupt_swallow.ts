import { workflow } from "@cleat/ts";

// SPIKE ONLY (cleat#3922). A guest that tries to survive the interrupt: a `catch` and a `finally` around the loop.
// If the interrupt is uncatchable and unwinds past `finally`, neither host call inside them is recorded.
export const interruptSwallow = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  try {
    for (let i = 0; ; i++) {
      if (i < 0) break;
    }
  } catch (e) {
    await ctx.call("inventory", "caught", {});
    return { caught: true };
  } finally {
    await ctx.call("inventory", "finally", {});
  }
  return { finished: true };
});
