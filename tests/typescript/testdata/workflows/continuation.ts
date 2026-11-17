import { workflow } from "@cleat/ts";

// cleat#3827, group G.

// Counts up by continuing as new until it reaches `limit`, then returns where it got to. The statement AFTER the continueAsNew call
// records a promise, so if the call returned (as the host's does) the history would carry an event for a run that has been replaced.
export const continuation = workflow<{ n: number; limit: number }, { n: number }>(async (ctx, input) => {
  if (input.n < input.limit) {
    await ctx.continueAsNew({ n: input.n + 1, limit: input.limit });
    await ctx.createPromise("reached-past-continue-as-new"); // unreachable: the call never settles
  }
  return { n: input.n };
});

// The versioned form. Whether a worker honours the version is the real-worker test's question; in-process the engine records it.
export const continuationVersioned = workflow<{ version: number }, null>(async (ctx, input) => {
  await ctx.continueAsNew({ carried: true }, { version: input.version });
  return null;
});

// A version that cannot be one: each is caught, so one run reports all of them.
export const continuationBadVersion = workflow<Record<string, never>, Record<string, string>>(async (ctx) => {
  const messageOf = async (v: number): Promise<string> => {
    try {
      await ctx.continueAsNew({}, { version: v });
      return "";
    } catch (e) {
      return (e as Error).message;
    }
  };
  return { zero: await messageOf(0), negative: await messageOf(-3), fractional: await messageOf(1.5), nan: await messageOf(NaN) };
});

// A continueAsNew AFTER the call that suspended the segment, not awaited. Unguarded it would record a continue_as_new event and
// REPLACE the sleep's suspension reason, so the run would restart instead of waiting.
export const continuationLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.continueAsNew({ n: 1 });
  await p;
  return null;
});

// cleat-review's probe on cleat#4043: a continueAsNew the host ACCEPTED, not awaited, followed by another guarded call. The host does not
// mark the segment suspended on a continuation, so unless the binding does, listCrons reaches the host and records an event for a run
// that has been replaced. (listCrons is read-only; a scheduleCron or a send in its place would be a real side effect.)
export const continuationThenAnotherCall = workflow(async (ctx) => {
  ctx.continueAsNew({ n: 1 });
  await ctx.listCrons();
  return null;
});

// The same probe through the versioned form, which has its own binding and its own mark.
export const continuationVersionedThenAnotherCall = workflow(async (ctx) => {
  ctx.continueAsNew({ n: 1 }, { version: 2 });
  await ctx.listCrons();
  return null;
});
