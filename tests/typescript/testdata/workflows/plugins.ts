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
  // The chunk array the host emits, typed as such: an earlier draft of these bindings documented a joined string, and the
  // type is what stops that mistake coming back.
  const streamed = await ctx.pluginCallStreaming("demo", "stream", {});
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

// An update delivered while the workflow is BLOCKED IN A WAIT (cleat#3827 group I, with group A's dispatch). `waitForEvent`
// is one of the fixed program positions an update can be delivered at; without `dispatchThen` around it, the update was
// pending the whole time and the workflow answers "no update handler registered" for it.
export const pluginWaitUpdate = workflow(async (ctx) => {
  let total = 0;
  await ctx.registerUpdateHandler<{ n: number }, { total: number }>("add", (input) => {
    total += input.n;
    return { total };
  });
  // Unclaimable (`found: false`) with a long budget, so the workflow suspends inside the wait and the delivery has to
  // happen at the dispatch point in front of it.
  await ctx.waitForEvent("demo", "claim", ["evt"], { timeoutMs: 60_000 });
  return { total };
});

// A budget the host cannot honour must be refused rather than passed through: NaN becomes 0 across the ABI's f64 -> i64
// conversion and a negative value makes the first `remaining <= 0` test fire, so both silently mean "no wait at all" --
// a timeout reported for a workflow that asked to wait.
export const pluginWaitBadTimeout = workflow(async (ctx) => {
  const messages: string[] = [];
  for (const timeoutMs of [Number.NaN, -1, Number.POSITIVE_INFINITY]) {
    try {
      await ctx.waitForEvent("demo", "claim", ["evt"], { timeoutMs });
      messages.push("ACCEPTED " + String(timeoutMs));
    } catch (e) {
      messages.push((e as Error).message);
    }
  }
  return { messages };
});
