import { workflow } from "../../packages/cleat-ts/index";

// Phase 2 durability: a run killed in the MIDDLE of executing (not while suspended). `burn` is a fixed-length CPU loop with no host
// call in it, so a SIGKILL during it lands inside guest execution. `spinFirst` puts it before the first recorded host call, so
// the kill leaves no history at all; otherwise it runs after a recorded side effect.
function burn(n: number): number {
  let x = 0;
  for (let i = 0; i < n; i++) x += Math.sqrt(i);
  return Math.round(x);
}

export const busy = workflow<{ iters: number; spinFirst?: boolean }, Record<string, unknown>>(async (ctx, input) => {
  let sum = 0;
  if (input.spinFirst) sum = burn(input.iters);
  const a = await ctx.sideEffect(() => Math.random());
  if (!input.spinFirst) sum = burn(input.iters);
  await ctx.sleep(500);
  return { a, sum, workflow: ctx.workflowId() };
});
