import { workflow } from "@cleat/ts";

// The detached-group bindings (cleat#3827, group D): start a workflow nobody waits for.
//
// `background` is deliberately a name the test's store does NOT script, so the run it creates stays RUNNING. That is the
// assertion that matters: if either binding waited for the run it started, this workflow would suspend, and a test that only
// checked the return value would still pass.

export const detachedRun = workflow(async (ctx) => {
  await ctx.runDetached("background", { n: 1 });
  return { reached: true };
});

export const detachedStart = workflow(async (ctx) => {
  const runId = await ctx.startDetached("background", { n: 2 });
  return { runId, reached: true };
});
