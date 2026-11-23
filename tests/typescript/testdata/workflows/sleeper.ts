import { workflow } from "@cleat/ts";

// Q2: a durable sleep. The first segment suspends at the sleep; the next replays from the start with the history and finishes.
export const sleeper = workflow<{ ms?: number }, { slept: number; workflow: string }>(async (ctx, input) => {
  const ms = input?.ms ?? 3000;
  await ctx.sleep(ms);
  return { slept: ms, workflow: ctx.workflowId() };
});
