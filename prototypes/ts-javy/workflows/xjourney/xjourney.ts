import { workflow } from "../../packages/cleat-ts/index";

// Q4, suspension events: the TypeScript twin of gocontrol.XJourney. A sleep (records no event), a signal wait, and a child, with a
// result that has no run-specific ids so it can be compared byte for byte with the Go twin's.
export const xJourney = workflow<{ sleepMs?: number; childMs: number }, { signal: unknown; slept: number }>(async (ctx, input) => {
  if ((input.sleepMs ?? 0) > 0) await ctx.sleep(input.sleepMs!);
  const sig = await ctx.waitForSignal("go", 10 * 60 * 1000);
  const child = (await ctx.child("xsleeper", { ms: input.childMs })) as { slept: number };
  return { signal: sig.payload, slept: child.slept };
});
