import { workflow } from "@cleat/ts";

// Makes calls-group calls AFTER the call that suspends the segment. A segment records nothing past the suspend point, so none
// may reach the host: an unguarded binding records a durable_send or a call event -- and, for `send`, dispatches a real request
// -- for a segment the host is about to abandon.
export const lateCalls = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.send("email", "notify", {});
  ctx.callRetry("payments", "charge", {});
  await p;
  return { reached: true };
});
