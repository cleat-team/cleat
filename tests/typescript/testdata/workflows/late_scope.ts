import { workflow } from "@cleat/ts";

// Calls a scope setter AFTER the call that suspends the segment, and never awaits it. The plugin's contract is that a
// segment records nothing past the call that suspended it, so the set below must not reach the host: with the binding
// unguarded it does, and a ScopeAcquired event is recorded for a scope the run does not hold. See
// TestTypeScriptACallAfterASuspensionRecordsNothing, which reads the history rather than the return value.
// The ABI entry is `late_scope`; build.sh generates a wrapper calling the CAMELCASE name, so the source must export
// lateScope. A source that exported the snake_case name builds a wrapper around `undefined` and fails at run time
// with "not a function" — which is how this file was first written.
export const lateScope = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.setScope("after", "a1");
  await p;
  return { reached: true };
});
