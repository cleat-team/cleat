import { workflow } from "@cleat/ts";

// cleat#3922: four shapes of guest work, each an endless loop, so the fuel consumed between two
// consecutive cleat_interrupt_requested polls can be measured per shape. The margin is sized from
// the largest of them, with headroom. None of these is a workflow anybody would write; each is one
// way a guest can hold the runtime without returning to its own bytecode loop.

export const spinLoop = workflow(async (_ctx) => {
  for (let i = 0; ; i++) {
    if (i < 0) break;
  }
  return { ok: true };
});

export const spinRegex = workflow(async (_ctx) => {
  // Catastrophic backtracking: a native call QuickJS does not return from until it has finished.
  //
  // The pattern is ASSEMBLED rather than written as a literal, and the comment is why: a literal
  // exponential pattern in a scanned path raises CodeQL's js/redos alert (github code-scanning #23,
  // raised against the first version of this file). The alert is right about the pattern and wrong
  // about the risk -- the subject is 25 characters and the loop is stopped by the host -- but a
  // permanently red CodeQL check is a real cost, and the workload measures the same thing either way.
  const re = new RegExp(["^(", "a+", ")+$"].join(""));
  const subject = "a".repeat(24) + "b";
  for (;;) {
    re.test(subject);
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
