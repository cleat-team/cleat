import { workflow } from "../../packages/cleat-ts/index";

// Plan 4.4: a `finally` must not run when the workflow suspends, and must run once when it completes. The trace goes to
// stderr (WASI fd_write, allowed by the policy), so the test reads it from the worker's log.
export const finalizer = workflow<unknown, { done: boolean }>(async (ctx) => {
  try {
    await ctx.sleep(1500);
    return { done: true };
  } finally {
    console.error("TRACE finalizer finally-ran workflow=" + ctx.workflowId());
  }
});
