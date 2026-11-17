import { workflow } from "@cleat/ts";

// cleat#3827, group B. Polls without waiting: a name with a delivery, the same name again (a poll does not consume), and a
// name nobody sent. The three answers are returned so the test reads what the guest saw, not only what the store holds.
export const signalsPoll = workflow<Record<string, never>, { first: unknown; again: unknown; none: unknown }>(async (ctx) => {
  const first = await ctx.pollSignal("go");
  const again = await ctx.pollSignal("go");
  const none = await ctx.pollSignal("nobody-sent-this");
  return { first, again, none };
});

// A signal polled and then WAITED for: the wait receives (and consumes) what the poll only looked at.
export const signalsPollThenWait = workflow<Record<string, never>, { polled: unknown; waited: unknown }>(async (ctx) => {
  const polled = await ctx.pollSignal("go");
  const waited = await ctx.waitForSignal("go", 1000);
  return { polled, waited };
});

// Sends: to another run, to itself, with no payload, and with a string. The target is an input so the test can name the run.
export const signalsSend = workflow<{ target: string }, { me: string; other: unknown }>(async (ctx, input) => {
  await ctx.signalWorkflow(input.target, "hello", { n: 1 });
  await ctx.signalWorkflow(input.target, "bare");
  await ctx.signalWorkflow(ctx.runId(), "to-me", "text");
  const back = await ctx.pollSignal("to-me");
  // A send to ANOTHER run must not be readable back by this one: polled here, it is undefined, so a store that queued every send
  // regardless of its target would put {n:1} here and the test would see it.
  const other = await ctx.pollSignal("hello");
  return { me: back as string, other };
});

// A send and a poll AFTER the call that suspended the segment, neither awaited. The send must not reach the host (it would
// record a signal_received event for a send the run never reached); the poll records nothing either way.
export const signalsLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.signalWorkflow("somebody", "late", 1);
  ctx.pollSignal("go");
  await p;
  return null;
});
