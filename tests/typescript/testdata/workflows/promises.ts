import { workflow } from "@cleat/ts";

// The durable promise bindings (cleat#3827). The promises here are settled by the STORE the test configures, standing in
// for another run: a promise exists so that something other than the waiter can complete it, so a test that resolved its
// own promise from the workflow would not be testing that.
//
// Two entries, because one workflow cannot both finish and suspend: `promises` completes (every promise it waits on is
// already settled), and `promiseTimeout` ends by waiting on one that never is.

export const promises = workflow(async (ctx) => {
  const id = await ctx.createPromise("answered");
  const got = await ctx.awaitPromise<{ greeting: string }>(id, 5000);

  const rejectedId = await ctx.createPromise("refused");
  let rejection = "";
  try {
    await ctx.awaitPromise(rejectedId, 5000);
  } catch (e) {
    rejection = (e as Error).message;
  }

  // A promise the workflow settles ITSELF, to exercise resolvePromise's success path: the store accepts the value, and
  // the wait that follows reads it back. Without this, only resolvePromise's error path is ever run.
  const mine = await ctx.createPromise("pending");
  await ctx.resolvePromise(mine, { v: 7 });
  const mineGot = await ctx.awaitPromise<{ v: number }>(mine, 5000);

  // Both setters against an id no store knows: each must surface the host's error rather than resolving quietly.
  let resolveError = "";
  try {
    await ctx.resolvePromise("no-such-promise", { v: 1 });
  } catch (e) {
    resolveError = (e as Error).message;
  }
  let rejectError = "";
  try {
    await ctx.rejectPromise("no-such-promise", "nothing to refuse");
  } catch (e) {
    rejectError = (e as Error).message;
  }

  return {
    id: id,
    value: got.value,
    timedOut: got.timedOut,
    rejection: rejection,
    mineValue: mineGot.value.v,
    resolveError: resolveError,
    rejectError: rejectError,
  };
});

// Waits on a promise nothing will settle, with a short timeout, and reports whether the wait timed out. The first pass
// SUSPENDS (the host answers the stop sentinel, not a timeout); once the deadline has really passed, the replay reaches
// the timeout branch and this returns {timedOut: true}.
// The ABI entry is `promise_timeout`; build.sh wraps the CAMELCASE export (cleat#3911).
export const promiseTimeout = workflow(async (ctx) => {
  const id = await ctx.createPromise("pending");
  const r = await ctx.awaitPromise<unknown>(id, 200);
  return { timedOut: r.timedOut, value: r.value };
});
