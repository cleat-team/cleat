import { CleatError, workflow } from "@cleat/ts";

// The children-group bindings (cleat#3827, group C): start with options, await all, await any, and poll.
//
// Every export starts children through the harness's store, which scripts an outcome by the child's NAME and leaves an
// unscripted name RUNNING. That is the only lever this process has: a real child is a separate run, so "still running" is
// what a name the script does not mention returns, and it is what makes the suspend cases reachable at all.

export const childrenAll = workflow(async (ctx) => {
  // Started in one order, awaited in another on purpose: the host answers in the order the ids are GIVEN, and a layer that
  // quietly sorted them or keyed the outcomes by run id would still pass a test that started and awaited in the same order.
  const alpha = await ctx.startChildWithOptions<{ n: number }>("alpha", { n: 1 });
  const bad = await ctx.startChildWithOptions<{ n: number }>("bad", { n: 2 });
  const outcomes = await ctx.awaitAll<{ v: number }>([bad, alpha]);
  return {
    alpha,
    bad,
    outcomes: outcomes.map((o) =>
      o.status === "fulfilled" ? { runId: o.runId, status: o.status, value: o.value } : { runId: o.runId, status: o.status, reason: o.reason.message },
    ),
  };
});

export const childrenAllSuspends = workflow(async (ctx) => {
  // "alpha" is scripted, "held" is not, so awaitAll has one child that will never finish in this segment. The workflow must
  // SUSPEND rather than report the running child as a failure -- the engine would otherwise record "not completed" as that
  // child's permanent answer.
  const alpha = await ctx.startChildWithOptions("alpha", {});
  const held = await ctx.startChildWithOptions("held", {});
  const outcomes = await ctx.awaitAll([alpha, held]);
  return { outcomes: outcomes.length };
});

export const childrenAny = workflow(async (ctx) => {
  const alpha = await ctx.startChildWithOptions<{ n: number }>("alpha", {});
  const held = await ctx.startChildWithOptions("held", {});
  const winner = await ctx.awaitAny<{ v: number }>([held, alpha]);
  return { runId: winner.runId, value: winner.value };
});

export const childrenAnyFails = workflow(async (ctx) => {
  const bad = await ctx.startChildWithOptions("bad", {});
  const held = await ctx.startChildWithOptions("held", {});
  try {
    const winner = await ctx.awaitAny([held, bad]);
    return { winner: winner.runId, message: "", childRunId: "" };
  } catch (e) {
    // childRunId is the tag the binding adds: without it a caller holding a bare rejection cannot tell which child failed.
    const err = e as Error & { childRunId?: string };
    return { winner: "", message: err.message, childRunId: err.childRunId ?? "" };
  }
});

export const childrenPoll = workflow(async (ctx) => {
  const alpha = await ctx.startChildWithOptions<{ v: number }>("alpha", {});
  const bad = await ctx.startChildWithOptions("bad", {});
  const held = await ctx.startChildWithOptions("held", {});
  // pollChild returns a promise (so a refused id arrives as a CleatError) even though it never waits: the promise settles at
  // once.
  const done = await ctx.pollChild<{ v: number }>(alpha);
  const failed = await ctx.pollChild(bad);
  const running = await ctx.pollChild(held);
  return {
    done,
    // A thrown host error would fail the workflow, so a snapshot here is also the assertion that a failed child is reported
    // as a snapshot and not as a thrown error (cleat#3387).
    failed: failed.status === "rejected" ? { status: failed.status, reason: failed.reason.message } : failed,
    running,
  };
});

// A module-level counter, so the test can see the handler RAN rather than only that the request was completed.
let handled = 0;

export const childrenWithUpdate = workflow(async (ctx) => {
  await ctx.registerUpdateHandler<{ n: number }>("bump", (u) => {
    handled += u.n;
    return handled;
  });
  const alpha = await ctx.startChildWithOptions("alpha", {});
  const outcomes = await ctx.awaitAll([alpha]);
  return { handled, outcomes: outcomes.length };
});

export const childrenWithoutHandler = workflow(async (ctx) => {
  // No handler registered: the dispatch point must still ANSWER the request, with the "no update handler registered" error.
  // An unanswered request leaves the caller holding a promise nothing settles, which is why every delivery is completed.
  const alpha = await ctx.startChildWithOptions("alpha", {});
  const outcomes = await ctx.awaitAll([alpha]);
  return { outcomes: outcomes.length };
});

// A host error, which is NOT a failed child (cleat#3387): the store cannot answer for a run id it never minted, and
// pollChild must reject with a CleatError rather than resolve with a snapshot of some status.
export const childrenPollUnknown = workflow(async (ctx) => {
  try {
    const snapshot = await ctx.pollChild("no-such-run");
    return { status: snapshot.status, message: "", isCleatError: false };
  } catch (e) {
    return { status: "", message: (e as Error).message, isCleatError: e instanceof CleatError };
  }
});
