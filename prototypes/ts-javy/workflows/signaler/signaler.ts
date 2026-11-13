import { workflow } from "../../packages/cleat-ts/index";

// Q2: waits for an external signal.
export const signaler = workflow<unknown, { got: unknown; timedOut: boolean }>(async (ctx) => {
  const s = await ctx.waitForSignal("go", 10 * 60 * 1000);
  return { got: s.payload, timedOut: s.timedOut };
});
