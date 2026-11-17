import { workflow } from "@cleat/ts";

// The smallest workflow that does something durable: two calls to a service, the second one fed by the first.
//
// Each `ctx.call` is recorded by the engine, so if the worker dies between the two, the replay does NOT call the greeter
// again: it replays the recorded answer and carries on from the second call.
//
// Nothing here uses a timer, `Date.now()`, `Math.random()`, `fetch` or `URL`. The build refuses those, because a workflow
// that read the clock would take a different path on replay. Use `ctx.now()`, `ctx.random()` and `ctx.call` instead.
export const greet = workflow(async (ctx, input: { name: string }) => {
  const composed = await ctx.call<{ text: string }>("greeter", "compose", { name: input.name });
  const audited = await ctx.call<{ id: string }>("audit", "record", { text: composed.text });
  return { greeting: composed.text, auditId: audited.id };
});
