import { workflow } from "../../packages/cleat-ts/index";

// Phase 4 (plan section 5): the benchmark workflow. n recorded host calls (sideEffect: one history event each; NOT random, which is
// a derived stream and records nothing, as the first draft of this file found), then an optional sleep that suspends it. The Go,
// Rust and Python twins (bench/) do exactly this, so every metric compares like with like.
//   n=1,  sleep_ms=0      cold start (instantiate + first host call)
//   n=1000, sleep_ms=60000  replay of a 1,000-step history to the suspension point
//   n=10, sleep_ms=0      throughput (sleep-free)
export const benchSteps = workflow<{ n: number; sleep_ms: number }, { sum: number }>(async (ctx, input) => {
  for (let i = 0; i < input.n; i++) await ctx.sideEffect(() => i);
  if (input.sleep_ms > 0) await ctx.sleep(input.sleep_ms);
  return { sum: input.n };
});
