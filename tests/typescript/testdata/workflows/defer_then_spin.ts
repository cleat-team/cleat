import { workflow } from "@cleat/ts";

// cleat#3826: a workflow that registers a defer and then never returns, so the host kills it (the instruction limit)
// and tries to run the defer itself. It cannot yet: see typescript_defer_kill_test.go for what is measured and what is not.
export const deferThenSpin = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  // A busy loop with no host call and no await: the only way out is the host killing the instance.
  for (let i = 0; ; i++) {
    if (i < 0) break;
  }
  return { ok: true };
});
