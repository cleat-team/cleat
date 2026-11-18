import { workflow } from "@cleat/ts";

// cleat#3922: endless loops, one per shape of guest work, so the budget consumed between two
// consecutive cleat_interrupt_requested polls can be measured per shape. The margins are sized from
// the largest, with headroom. None of these is a workflow anybody would write; each is one way a
// guest can hold the runtime without returning to its own bytecode loop.
//
// A deliberately exponential regex workload used to be here. It was dropped rather than assembled
// at runtime to keep CodeQL quiet: code scanning raised a high js/redos alert on the literal, the
// alert was right about the pattern and wrong about the risk, and hiding the decision from the
// scanner is worse than removing the shape. It was never load bearing -- 2.1M fuel against
// JSON.parse's 26.2M -- so no margin moved.

export const spinLoop = workflow(async (_ctx) => {
  for (let i = 0; ; i++) {
    if (i < 0) break;
  }
  return { ok: true };
});

export const spinJson = workflow(async (_ctx) => {
  const payload = JSON.stringify(Array.from({ length: 200000 }, (_, i) => ({ i, name: "x".repeat(20) })));
  for (;;) {
    JSON.parse(payload);
  }
  return { ok: true };
});

export const spinString = workflow(async (_ctx) => {
  for (;;) {
    const s = "x".repeat(10_000);
    s.slice(0, 1000);
  }
  return { ok: true };
});

export const spinNative = workflow(async (_ctx) => {
  for (;;) {
    Math.max(1, 2);
  }
  return { ok: true };
});

export const spinStringProbe = workflow(async (ctx) => {
  await ctx.call("probe", "start", {});
  for (;;) {
    const s = "x".repeat(10_000);
    s.slice(0, 1000);
  }
  return { ok: true };
});

export const spinStringBig = workflow(async (_ctx) => {
  for (;;) {
    const s = "x".repeat(1_000_000);
    s.slice(0, 1000);
  }
  return { ok: true };
});
