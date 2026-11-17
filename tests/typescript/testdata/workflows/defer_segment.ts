import { workflow } from "@cleat/ts";

// cleat#3826: the DEFER SEGMENT fixture, and the thing that lets `javascript` into deferSegmentLanguages.
//
// Two defers, then one call of the workflow's own. Run as a defer segment, the host refuses the body's call
// (stopBeforeNewWork), the guest unwinds on the suspend sentinel, and only the two defers reach the ServiceCaller --
// in LIFO order, because the second is registered last and runs first.
//
// The body call is NOT discarded here, unlike the Rust fixture's: this guest's bindings return a never-settling promise
// once the SDK's suspended flag is set, so `await` parks the body rather than throwing and there is nothing to catch.
// What ends the segment is the flag `cleat_take_result` reads, which is why the test asserts on the sentinel rather than
// on an error.
export const deferSegment = workflow(async (ctx) => {
  await ctx.defer(async () => {
    await ctx.call("inventory", "first", {});
  });
  await ctx.defer(async () => {
    await ctx.call("inventory", "second", {});
  });
  await ctx.call("inventory", "body", {});
  return { ok: true };
});
