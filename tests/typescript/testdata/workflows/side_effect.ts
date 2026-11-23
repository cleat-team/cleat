import { workflow } from "@cleat/ts";

// cleat#4113. One frozen value, and a count of how often its function ran in THIS execution. Every execution is a fresh instance, so the
// module-level counter starts at zero: a first run reports ran=1, and a replay that returns the recorded value WITHOUT calling the
// function reports ran=0.
let ran = 0;

export const sideEffectOnce = workflow<Record<string, never>, { value: string; ran: number }>(async (ctx) => {
  const value = await ctx.sideEffect(() => {
    ran += 1;
    return `computed-in-run-${ran}`;
  });
  return { value, ran };
});
