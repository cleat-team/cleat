import { workflow } from "@cleat/ts";

// The workflow metadata, scope and cancellation bindings (cleat#3827) in one pass. Every value here is compared against
// what the engine in typescript_host_calls_test.go was configured with, not against a literal written into this file: a
// workflow that returned its own expectation could not tell a wrong binding from a right one.
export const metadata = workflow<unknown, Record<string, unknown>>(async (ctx) => {
  const before = ctx.getScope();
  const prevFirst = await ctx.setScope("cart", "c1");
  const during = ctx.getScope();
  const prevSecond = await ctx.setScope("order", "o1");
  const after = ctx.getScope();
  return {
    version: ctx.version(),
    minVersion: ctx.minVersion(),
    runId: ctx.runId(),
    workflowId: ctx.workflowId(),
    cancellation: ctx.cancellation(),
    before: before,
    prevFirst: prevFirst,
    during: during,
    prevSecond: prevSecond,
    after: after,
  };
});
