import { workflow } from "@cleat/ts";

// cleat#3922: the residue. A loop whose work is dominated by native string allocation does not
// return to the QuickJS bytecode loop often enough to be polled, so the soft kill never reaches it
// and the hard trap does. Measured: no handler call at all inside a 400M fuel budget, and none in
// 2s of wall clock. See DIVERGENCES.md.
export const deferThenStringSpin = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "cleanup", {});
  });
  for (;;) {
    const s = "x".repeat(1_000_000);
    s.slice(0, 1000);
  }
  return { ok: true };
});
