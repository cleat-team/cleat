import { workflow } from "../../packages/cleat-ts/index";

// Q2: three suspension points in one run (a sleep, a signal, a child workflow that itself sleeps), plus the
// non-suspending determinism-relevant calls. Every resume replays the whole run from the start with its history.
// The durations are input so a verifier can hold the run in flight long enough to snapshot it: a completed run's
// event_history is deleted, so a snapshot taken after completion reads 0 rows and proves nothing.
export const journey = workflow<{ sleepMs?: number; childMs?: number }, Record<string, unknown>>(async (ctx, input) => {
  const id = ctx.workflowId();
  await ctx.sleep(input?.sleepMs ?? 1500);
  const signal = await ctx.waitForSignal("go", 10 * 60 * 1000);
  const child = await ctx.child("sleeper", { ms: input?.childMs ?? 1500 });
  return { id, signal: signal.payload, child, uuid: ctx.uuid("journey"), random: ctx.random(), now: ctx.now() > 0 };
});
