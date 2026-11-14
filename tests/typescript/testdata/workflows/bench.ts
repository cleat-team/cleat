import { workflow } from "@cleat/ts";

// The benchmark workflow: n recorded host calls (sideEffect records one history event each; `random` is a derived stream and
// records nothing, so a "1,000-step history" built from it would be empty and every language would look instantaneous), then an
// optional sleep that suspends the run. The Go twin in ../gobench does exactly this, so each metric compares like with like.
//   n=1,    sleep_ms=0      cold start: instantiate + one host call + finish
//   n=1000, sleep_ms=60000  replay of a 1,000-step history to the suspension point
export const benchSteps = workflow<{ n: number; sleep_ms: number }, { sum: number }>(async (ctx, input) => {
  for (let i = 0; i < input.n; i++) await ctx.sideEffect(() => i);
  if (input.sleep_ms > 0) await ctx.sleep(input.sleep_ms);
  return { sum: input.n };
});
