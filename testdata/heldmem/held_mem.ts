import { workflow } from "@cleat/ts";

// MEASUREMENT guest (cleat#4377, residency memory budget): `calls` durable calls, `mb` MiB of touched live data, then EITHER a durable sleep of `sleep_ms` OR one blocking call named park.
const keep: Uint8Array[] = [];
export const heldMem = workflow<{ calls: number; mb: number; sleep_ms: number }, { kept: number }>(async (ctx, input) => {
  for (let i = 0; i < input.calls; i++) await ctx.call("work", "step", { stage: "step" });
  for (let i = 0; i < input.mb; i++) keep.push(new Uint8Array(1 << 20).fill(1));
  if (input.sleep_ms > 0) await ctx.sleep(input.sleep_ms);
  else await ctx.call("work", "park", { stage: "park" });
  await ctx.call("work", "done", { stage: "done" });
  return { kept: keep.length };
});
