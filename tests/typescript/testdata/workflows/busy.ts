import { workflow } from "@cleat/ts";

// A run killed in the MIDDLE of executing, not while suspended. `burn` is a fixed-length CPU loop with no host call in it, so a
// SIGKILL during it lands inside guest execution. It runs after a recorded side effect, so the run has a persisted history row
// before the burn starts. Math.sqrt is correctly rounded and the sum is taken in a fixed order, so the same input gives the same
// sum on any platform: that is what lets a resumed run be compared with an uninterrupted one.
function burn(n: number): number {
  let x = 0;
  for (let i = 0; i < n; i++) x += Math.sqrt(i);
  return Math.round(x);
}

export const busy = workflow<{ iters: number }, { a: number; sum: number; workflow: string }>(async (ctx, input) => {
  const a = await ctx.sideEffect(() => Math.random());
  const sum = burn(input.iters);
  return { a, sum, workflow: ctx.workflowId() };
});
