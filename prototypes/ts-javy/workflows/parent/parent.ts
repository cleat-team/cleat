import { workflow } from "../../packages/cleat-ts/index";

// Q2: starts a child workflow (which itself sleeps) and waits for it.
export const parent = workflow<{ ms?: number }, { child: unknown }>(async (ctx, input) => {
  const child = await ctx.child("sleeper", { ms: input?.ms ?? 3000 });
  return { child };
});
