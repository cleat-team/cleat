import { workflow } from "@cleat/ts";

// Makes ALL FOUR calls-group calls AFTER the call that suspends the segment, and never awaits them. A segment records nothing
// past the suspend point, so none may reach the host: an unguarded binding records a durable_send, a
// durable_schedule_invoke or a call event -- and, for send, dispatches a real request -- for a segment the host is about to
// abandon.
//
// ALL FOUR, because a guard is per binding and three of them record nothing a test can see unless the test makes them. Review
// measured exactly that on the first version: this file made only `send` and `callRetry`, and unguarding
// `__cleat_schedule_invoke` or `__cleat_call_heartbeat` left all seven tests green.
export const lateCalls = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.send("email", "notify", {});
  ctx.callRetry("payments", "charge", {});
  ctx.callHeartbeat("payments", "slow", {}, 1000);
  ctx.scheduleInvoke("email", "digest", 1000, {});
  await p;
  return { reached: true };
});
