import { workflow } from "@cleat/ts";

// Makes two children-group calls AFTER the call that suspends the segment, and never awaits them. The plugin's contract is
// that a segment records nothing past the call that suspended it, so neither must reach the host: with either binding
// unguarded it does, and the run starts a child it will never wait for (or records an await it never made) on a segment
// that is about to be abandoned. See TestTypeScriptAChildCallAfterASuspensionRecordsNothing, which reads the store and the
// history rather than the return value.
//
// BOTH SHAPES ARE HERE because they are guarded by the same mechanism and only one of them is a host call that suspends:
// `startChildWithOptions` writes a child_workflow event, and `awaitAny` dispatches pending updates before it waits -- so an
// unguarded binding reaches the host on a segment the host has already stopped.
export const lateChildren = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.startChildWithOptions("alpha", {});
  ctx.awaitAny(["child-1"]);
  await p;
  return { reached: true };
});
