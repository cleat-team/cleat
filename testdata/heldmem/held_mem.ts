import { workflow } from "@cleat/ts";

// MEASUREMENT guest (cleat#4162, residency memory budget): `calls` durable calls, `mb` MiB of touched live data, then one blocking call named park.
const keep: Uint8Array[] = [];
export const heldMem = workflow<{ calls: number; mb: number }, { kept: number }>(async (ctx, input) => {
  for (let i = 0; i < input.calls; i++) await ctx.call("work", "step", { stage: "step" });
  for (let i = 0; i < input.mb; i++) keep.push(new Uint8Array(1 << 20).fill(1));
  await ctx.call("work", "park", { stage: "park" });
  return { kept: keep.length };
});
