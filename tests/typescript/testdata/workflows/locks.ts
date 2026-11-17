import { workflow } from "@cleat/ts";

// cleat#3827, group E. The key is an input so the database-backed test can give each run its own and not collide with a
// previous run's row; the in-process test passes a fixed one.

// Acquire, acquire again (never re-entrant), release, acquire again, and release a key this run never held.
export const locksRoundTrip = workflow<{ key: string; other?: string }, Record<string, unknown>>(async (ctx, input) => {
  const first = await ctx.acquireLock(input.key, 60_000);
  const again = await ctx.acquireLock(input.key, 60_000);
  await ctx.releaseLock(input.key);
  const afterRelease = await ctx.acquireLock(input.key, 60_000);
  const result: Record<string, unknown> = { first, again, afterRelease };
  if (input.other !== undefined) {
    // A key somebody else holds: not acquired, and releasing it must not free it for anyone.
    result.heldElsewhere = await ctx.acquireLock(input.other, 60_000);
    await ctx.releaseLock(input.other);
    result.stillHeldElsewhere = !(await ctx.acquireLock(input.other, 60_000));
  }
  return result;
});

// Takes the key and parks on a long sleep, HOLDING it, so another run can be refused.
export const locksHolder = workflow<{ key: string }, { held: boolean }>(async (ctx, input) => {
  const held = await ctx.acquireLock(input.key, 600_000);
  await ctx.sleep(300_000);
  return { held };
});

// A second run for the same key: refused while the holder holds it, and releasing it frees nothing it does not own.
export const locksContender = workflow<{ key: string }, { first: boolean; afterRelease: boolean }>(async (ctx, input) => {
  const first = await ctx.acquireLock(input.key, 60_000);
  await ctx.releaseLock(input.key);
  const afterRelease = await ctx.acquireLock(input.key, 60_000);
  return { first, afterRelease };
});

// The TTL is honoured: held, refused, then (after the TTL has passed) taken again by the same run.
export const locksExpiry = workflow<{ key: string; ttlMs: number }, { first: boolean; again: boolean; afterExpiry: boolean }>(async (ctx, input) => {
  const first = await ctx.acquireLock(input.key, input.ttlMs);
  const again = await ctx.acquireLock(input.key, input.ttlMs);
  await ctx.sleep(input.ttlMs + 600);
  const afterExpiry = await ctx.acquireLock(input.key, 60_000);
  return { first, again, afterExpiry };
});

// A TTL no store can honour is an error, not a lock that is born expired.
export const locksBadTtl = workflow<{ ttlMs: number }, { message: string }>(async (ctx, input) => {
  try {
    await ctx.acquireLock("never-taken", input.ttlMs);
    return { message: "" };
  } catch (e) {
    return { message: (e as Error).message };
  }
});

// An acquire and a release AFTER the call that suspended the segment, neither awaited. Each records an event when it reaches
// the host, and a segment must record nothing past the call that suspended it.
export const locksLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.acquireLock("late", 60_000);
  ctx.releaseLock("late");
  await p;
  return null;
});
