import { workflow } from "@cleat/ts";

// The durable promise bindings (cleat#3827). The promises here are settled by the STORE the test configures, standing in
// for another run: a promise exists so that something other than the waiter can complete it, so a test that resolved its
// own promise would not be testing that.
export const promises = workflow(async (ctx) => {
  const id = await ctx.createPromise("answered");
  const got = await ctx.awaitPromise<string>(id, 5000);

  const rejectedId = await ctx.createPromise("refused");
  let rejection = "";
  try {
    await ctx.awaitPromise(rejectedId, 5000);
  } catch (e) {
    rejection = (e as Error).message;
  }

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

  return { id: id, value: got.value, timedOut: got.timedOut, rejection: rejection, resolveError: resolveError, rejectError: rejectError };
});
