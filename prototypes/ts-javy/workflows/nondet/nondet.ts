// cleat-ts: allow TS001 TS002 TS003 TS006 -- this workflow PROBES the runtime for exactly those globals (Q3 controls); it is never deployed as a real workflow
import { workflow, type Ctx } from "../../packages/cleat-ts/index";

// Q3 / plan §4.5: one nondeterminism source per run, observed twice. Segment 1 computes `first` and records it through
// sideEffect (the host stores it); the run then suspends on a sleep; segment 2 REPLAYS from the start, so `first` is
// computed again by the replaying module while sideEffect hands back the value segment 1 recorded. They are equal only
// if the source is replay-stable. `later` is computed after the sleep, in new (non-replayed) execution, and is expected
// to differ for a time- or random-derived source: it shows the source is live, so "equal" is not just "constant".

declare const performance: { now(): number } | undefined;
declare const crypto: { getRandomValues(a: Uint8Array): Uint8Array } | undefined;
declare const setTimeout: ((f: () => void, ms?: number) => unknown) | undefined;
declare const setInterval: ((f: () => void, ms?: number) => unknown) | undefined;
declare const WeakRef: unknown;
declare const FinalizationRegistry: unknown;

// A fixed-length busy loop (never "until the clock moves": a clock that is frozen on replay would spin forever).
function spin(): number {
  let x = 0;
  for (let i = 0; i < 3000000; i++) x += Math.sqrt(i);
  return x;
}

const SOURCES: Record<string, (ctx: Ctx) => unknown> = {
  dateNow: () => Date.now(),
  newDate: () => new Date().getTime(),
  // Time measured across a busy loop, with no host call between the two reads. Live it is positive; if the clock is a frozen
  // virtual time on replay it is 0, so a branch on elapsed time takes a different path on replay than it did live.
  dateElapsed: () => {
    const a = Date.now();
    spin();
    return Date.now() - a > 0;
  },
  newDateElapsed: () => {
    const a = new Date().getTime();
    spin();
    return new Date().getTime() - a > 0;
  },
  dateEqualsHostNow: (ctx) => Date.now() === ctx.now(),
  // KNOWN LIMIT of tools/tscheck: a computed property access is not seen statically. At run time the global is absent, so the
  // call throws a deterministic TypeError: the runtime is the second layer.
  setTimeoutComputed: () => (globalThis as any)["set" + "Timeout"](() => {}, 1),
  dateCall: () => typeof Date() + ":" + (Date().length > 0),
  random: () => Math.random(),
  // 400 draws summarised: a generator that is merely replay-stable but constant, negative or lopsided must not pass.
  randomRange: () => {
    const v: number[] = [];
    for (let i = 0; i < 400; i++) v.push(Math.random());
    return { min: Math.min(...v), max: Math.max(...v), mean: v.reduce((a, b) => a + b, 0) / v.length, distinct: new Set(v).size };
  },
  perfNow: () => (typeof performance === "undefined" ? "absent" : performance.now()),
  cryptoRandom: () => (typeof crypto === "undefined" ? "absent" : Array.from(crypto.getRandomValues(new Uint8Array(4))).join(",")),
  // Locale and timezone: host-dependent in most runtimes. Recorded, not shimmed; see PHASE2.md for what QuickJS does.
  intl: () => (typeof (globalThis as Record<string, unknown>).Intl),
  toLocaleDate: () => new Date(0).toLocaleString(),
  toLocaleNumber: () => (1234567.891).toLocaleString(),
  weakRef: () => typeof WeakRef,
  finalizationRegistry: () => typeof FinalizationRegistry,
  setTimeout: () => (typeof setTimeout === "undefined" ? "absent" : "present"),
  setInterval: () => (typeof setInterval === "undefined" ? "absent" : "present"),
  setTimeoutCall: () => {
    if (typeof setTimeout === "undefined") return "absent";
    setTimeout(() => {}, 0);
    return "callable";
  },
};

function observe(f: (ctx: Ctx) => unknown, ctx: Ctx): string {
  try {
    return JSON.stringify(f(ctx));
  } catch (e) {
    return "threw:" + (e as Error).name;
  }
}

export const nondet = workflow<{ source: string; inject?: string[]; sleepMs?: number }, Record<string, unknown>>(async (ctx, input) => {
  // Known-positive for the absence checks: define the named globals here, so a check that expects "absent" can be shown to
  // notice one that is present. This runs in user code, after the plugin's own lockdown, so it is not itself a shim test.
  for (const k of input.inject ?? []) (globalThis as Record<string, unknown>)[k] = k === "performance" ? { now: () => 0 } : function () {};
  const f = SOURCES[input.source];
  if (!f) throw new Error("unknown source " + input.source);
  const first = observe(f, ctx);
  const recorded = await ctx.sideEffect(() => first);
  await ctx.sleep(input.sleepMs ?? 1200);
  const later = observe(f, ctx);
  return { source: input.source, recorded, replayed: first, same: recorded === first, later };
});
