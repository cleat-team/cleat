import { workflow } from "../../packages/cleat-ts/index";

// Plan 4.4: Promise.race and Promise.all over a suspended promise and a ready one. The history must be ordered the same
// way on every replay, and the result must not depend on which segment it was computed in.
export const racer = workflow<unknown, { race: string; all: string[] }>(async (ctx) => {
  const ready = Promise.resolve("ready");
  const slept = ctx.sleep(1500).then(() => "slept");
  const race = await Promise.race([slept, ready]);
  const all = await Promise.all([ready, slept, ctx.sleep(500).then(() => "slept-again")]);
  return { race, all };
});
