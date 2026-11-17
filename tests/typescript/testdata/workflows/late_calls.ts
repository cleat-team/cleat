import { workflow } from "@cleat/ts";

// Makes ALL FOUR calls-group calls AFTER the call that suspends the segment, and never awaits them. A segment records nothing
// past the suspend point, so none may reach the host: an unguarded binding records a durable_send, a
// durable_schedule_invoke or a call event -- and, for send, dispatches a real request -- for a segment the host is about to
// abandon.
//
// ALL FOUR, because a guard is per binding and each of the four leaves a different record behind: `callRetry` and
// `callHeartbeat` write a call event, `send` a durable_send event and `scheduleInvoke` a durable_schedule_invoke event.
// Unguarding any one of them individually fails the test -- measured one at a time, with REBUILD_PLUGIN=1.
export const lateCalls = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.send("email", "notify", {});
  ctx.callRetry("payments", "charge", {});
  ctx.callHeartbeat("payments", "slow", {}, 1000);
  ctx.scheduleInvoke("email", "digest", 1000, {});
  await p;
  return { reached: true };
});
