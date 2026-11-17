import { workflow } from "@cleat/ts";

// Makes both detached-group calls AFTER the call that suspends the segment, and never awaits them. A segment records nothing
// past the call that suspended it, so neither may reach the host: an unguarded binding starts a real workflow, and records a
// run_detached event, for a segment the host is about to abandon -- work nobody asked for and nothing will track.
//
// See TestTypeScriptADetachedCallAfterASuspensionRecordsNothing, which reads the store and the history rather than the
// return value.
export const lateDetached = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.runDetached("background", {});
  ctx.startDetached("background", {});
  await p;
  return { reached: true };
});
