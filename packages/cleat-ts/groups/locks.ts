// The `locks` group of host-call bindings (cleat#3827): `acquireLock` and `releaseLock`, a durable mutual-exclusion key with a
// time to live, shared by every run in the tenant.
//
// NEITHER CALL WAITS, so neither is a dispatch point and neither goes through `dispatchThen` (the update group's hook for calls
// that can park the workflow). `acquireLock` answers at once: there is no queue, and a caller that wants to wait for the key
// must loop with a `sleep` of its own.

import type { Helpers } from "./helpers";

declare function __cleat_acquire_lock(key: string, ttlMs: number): string;
declare function __cleat_release_lock(key: string): string;

export interface LocksCtx {
  /**
   * Try to take the lock `key` for at most `ttlMs` milliseconds. Resolves `true` when this run now holds it and `false` when
   * something else does, so "not acquired" is an ordinary answer and not an error. It rejects with a `CleatError` only when the
   * host cannot answer (a refused key, a store failure, or a `ttlMs` below 1, which every other SDK would pass through to a lock
   * that is born expired).
   *
   * NEVER RE-ENTRANT: asking for a key this run already holds answers `false`, on all three databases, because there is no hold
   * count and a second acquire followed by one release would leave the key free while the run believed it held it. And it does
   * NOT WAIT: a `false` is the answer now, not after the other holder finishes.
   *
   * The key expires after `ttlMs` whether or not it is released, so a run that crashes cannot hold it forever and a run that
   * outlives its TTL no longer holds it, with nothing to tell it so. Size the TTL to the longest the critical section can take.
   *
   * On replay the recorded answer is returned and the store is not asked again, so a run that was refused the lock is refused it
   * again however the store has changed since. A worker with NO concurrency-key store answers `false` to every acquire; the
   * test harness does the same unless a `locks` script is supplied.
   */
  acquireLock(key: string, ttlMs: number): Promise<boolean>;
  /**
   * Release `key` if this run holds it. Resolves either way: releasing a key that has expired, or that another run holds, is not
   * an error and does nothing (the engine records `LockNotHeld` in the history, which the guest cannot see). Only a run's own
   * hold is ever released.
   */
  releaseLock(key: string): Promise<void>;
}

export function makeLocks(h: Helpers): LocksCtx {
  return {
    acquireLock: (key, ttlMs) => h.settle<boolean>(__cleat_acquire_lock(key, ttlMs)),
    releaseLock: (key) => h.settle<null>(__cleat_release_lock(key)).then(() => undefined),
  };
}
