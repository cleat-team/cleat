import { workflow } from "@cleat/ts";

// The plugin-call bindings (cleat#3827 group I). Every function here is answered by the SCRIPTED registry the test
// configures -- there is no real plugin in this process -- so what is under test is the binding, not a plugin.
//
// The registry is keyed `<plugin>/<function>`. `demo/echo` answers a result, `demo/stream` answers chunks, and
// `demo/claim` is claim-shaped: it reports `found` so the host's claim loop can decide whether to wait.
//
// The ABI entry is the SNAKE_CASE name (`plugin_calls` and so on) and build.sh generates a wrapper calling the
// CAMELCASE export, so each source below is the camelCase form of its entry: pluginCalls, pluginWaitFound,
// pluginWaitTimesOut, pluginLate.

export const pluginCalls = workflow(async (ctx) => {
  const echo = await ctx.pluginCall<{ said: string }>("demo", "echo", { n: 1 });
  const streamed = await ctx.pluginCallStreaming<{ joined: string }>("demo", "stream", {});
  return { echo, streamed };
});

// A claim-shaped function that reports `found: true` on the first attempt. The wait must return that answer rather
// than registering and waiting.
export const pluginWaitFound = workflow(async (ctx) => {
  const event = await ctx.waitForEvent<{ found: boolean; payload: string }>("demo", "claim", ["evt"], {
    timeoutMs: 5000,
  });
  return { event };
});

// The other exit of the same loop: `found: false` with a zero budget, so the host times out instead of registering.
// The binding must surface that as a rejection -- the host reports a timeout as a call failure -- and the workflow
// records the message rather than failing, so the test can assert on it.
export const pluginWaitTimesOut = workflow(async (ctx) => {
  let message = "";
  try {
    await ctx.waitForEvent("demo", "claim", ["evt"], { timeoutMs: 0 });
  } catch (e) {
    message = (e as Error).message;
  }
  return { message };
});

// Calls AFTER the call that suspends the segment, mirroring late_scope.ts: the plugin contract is that a segment
// records nothing past the call that suspended it, so with the bindings unguarded this run records plugin calls it
// does not own. See TestTypeScriptAPluginCallAfterASuspensionRecordsNothing.
export const pluginLate = workflow(async (ctx) => {
  const p = ctx.sleep(60000);
  ctx.pluginCall("demo", "echo", { after: "sleep" });
  ctx.pluginCallStreaming("demo", "stream", {});
  await p;
  return { reached: true };
});
