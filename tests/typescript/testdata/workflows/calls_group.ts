import { workflow } from "@cleat/ts";

// The calls-group bindings (cleat#3827, group H): retry, heartbeat, send, scheduleInvoke.
//
// Every service name here is answered by the test's own scripted caller, which for `payments/charge` FAILS on the first attempt
// and succeeds on the second: a caller that only ever succeeded would let a binding that never retried pass.

export const callsRetry = workflow(async (ctx) => {
  const out = await ctx.callRetry<{ id: string }>(
    "payments",
    "charge",
    { amount: 7 },
    { maxAttempts: 3, initialIntervalMs: 1, backoffCoefficient: 2, maxIntervalMs: 4 },
  );
  return { out };
});

export const callsRetryExhausted = workflow(async (ctx) => {
  try {
    await ctx.callRetry("payments", "always-fails", {}, { maxAttempts: 2, initialIntervalMs: 1, maxIntervalMs: 2 });
    return { message: "" };
  } catch (e) {
    return { message: (e as Error).message };
  }
});

export const callsHeartbeat = workflow(async (ctx) => {
  const out = await ctx.callHeartbeat<{ alive: boolean }>("payments", "slow", { job: 1 }, 50);
  return { out };
});

export const callsSend = workflow(async (ctx) => {
  await ctx.send("email", "notify", { to: "someone@example.test" });
  return { reached: true };
});

export const callsSchedule = workflow(async (ctx) => {
  await ctx.scheduleInvoke("email", "digest", 250, { every: "day" });
  return { reached: true };
});
