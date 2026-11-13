// Phase 5 / Q6: how a case from an npm package is run. Computed once, recorded through sideEffect (so a replay that computes a
// different value is visible as recorded != replayed), then the run suspends on a sleep and computes it again in new execution
// (laterSame: is the case a pure function of its inputs at all). The same CASES table is run directly under Node for the expected
// value (phase5_npm.py), so "ran unmodified" means: the guest's answer is Node's answer, byte for byte.
import type { Ctx } from "../packages/cleat-ts/index";

export type Cases = Record<string, () => unknown>;

export function observe(f: () => unknown): string {
  try {
    const v = f();
    return String(JSON.stringify(v === undefined ? "undefined" : v));
  } catch (e) {
    return "threw:" + (e as Error).name + ":" + String((e as Error).message).slice(0, 160);
  }
}

export async function runCase(ctx: Ctx, name: string, cases: Cases) {
  const f = cases[name];
  if (!f) throw new Error("unknown case " + name);
  const first = observe(f);
  const recorded = await ctx.sideEffect(() => first);
  await ctx.sleep(300);
  const later = observe(f);
  return { name, recorded, replaySame: recorded === first, laterSame: later === first };
}
