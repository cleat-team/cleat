import { workflow } from "@cleat/ts";

// A workflow that waits for a signal the TEST delivered before it started (cleat#3828).
//
// The signal is the store's job, not the workflow's: a workflow cannot signal itself and then claim it waited, so the
// fixture's only job is to block on one and return what arrived. The two directions the harness test needs come from
// the SCRIPT rather than from this file -- scripted, the wait returns the payload and the workflow finishes; unscripted,
// the same wait parks, which is what shows the store was what made the first case complete.
//
// The timeout is long because it should never be reached: a signal already in the queue is answered on the first poll.
// A short one would turn a broken store into a fast `timedOut: true` and read as a different result rather than a hang.
export const signalStore = workflow(async (ctx) => {
  const s = await ctx.waitForSignal("approve", 300000);
  return { name: s.name, payload: s.payload, timedOut: s.timedOut };
});
