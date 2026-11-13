import { workflow } from "../../packages/cleat-ts/index";

// Phase 1 decision (4): error and exception paths. Each entry is one way a guest can go wrong; the verifier records
// what the run ends as, what the error text says, and whether the engine's WASI policy was ever reached.

// A plain throw inside the workflow: the SDK's catch reports it through __cleat_fail.
export const thrower = workflow(async () => {
  throw new Error("thrower: deliberate failure");
});

// A rejection that is not an Error, after an await of an already-settled promise.
export const rejecter = workflow(async () => {
  await Promise.resolve(1);
  return await Promise.reject("rejecter: a string, not an Error");
});

// Suspends first, throws on the RESUMED segment: the failure happens during replay-then-new-work, not on the first run.
export const lateThrower = workflow(async (ctx) => {
  await ctx.sleep(1500);
  throw new Error("lateThrower: failed after a resume");
});

// An exception that escapes entirely: no workflow() wrapper, so nothing in the SDK catches it. This is what a
// top-level bug in guest code looks like to Javy and to the engine.
export const rawThrow = (): void => {
  throw new Error("rawThrow: nothing catches this");
};

// Memory exhaustion: grow until something refuses. Chunks are distinct strings so nothing is shared or collapsed.
export const oom = workflow(async () => {
  const keep: string[] = [];
  for (let i = 0; ; i++) keep.push(("x" + i).repeat(1 << 16));
});
