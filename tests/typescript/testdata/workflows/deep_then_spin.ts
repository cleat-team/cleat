import { workflow } from "@cleat/ts";

// cleat#3922: an expensive UNWIND. The guest descends a deep stack, then spins; the soft kill has to
// unwind every frame, which costs more fuel than the ordinary shapes measured. The fuel reserve must
// cover that, or the hard trap fires mid-unwind and the defer is lost again.
export const deepThenSpin = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  const descend = (n: number): void => {
    if (n === 0) {
      for (let i = 0; ; i++) {
        if (i < 0) break;
      }
    }
    descend(n - 1);
  };
  descend(500);
  return { ok: true };
});
