// The `detached` group of host-call bindings (cleat#3827): start a workflow nobody waits for.
//
// A DETACHED RUN IS NOT A CHILD. Nothing awaits it, its failure is not this workflow's failure, and finishing here does not
// finish it -- `Ctx.child`, `awaitAll` and `awaitAny` are the calls for a run whose result this workflow needs. The two calls
// below differ only in whether the run id is handed back: `runDetached` is fire-and-forget, `startDetached` gives a handle for
// polling, signalling or recording it somewhere durable.
//
// NEITHER IS A DISPATCH POINT, and that is a measured fact rather than an omission. Neither call can park the workflow: the one
// sentinel they can return is the defer-phase refusal, which cleat-sdk reports as an ERROR (it asks whether the host stopped
// the call before decoding the result, because the stop bit is not a field of a simple result -- HostCalls::run_detached). So
// there is nothing to dispatch updates before.
//
// REPLAY. Both record a `run_detached` event carrying the name, the input and the run id, and on replay both match that event
// and hand back the RECORDED id rather than starting a second run. The name is part of the match, so a reordered pair of
// detached starts is a replay divergence rather than a silently swapped run.

import type { Helpers } from "./helpers";

declare function __cleat_run_detached(name: string, input: string): string;
declare function __cleat_start_detached(name: string, input: string): string;

export interface DetachedCtx {
  /**
   * Start the workflow `name` detached, and resolve once the host has taken it. There is no result to wait for: the run is not
   * a child, is not awaited, and its outcome never reaches this workflow.
   *
   * REFUSED IN A DEFER PHASE, which exists to clean up and not to start work. The refusal arrives as a `CleatError` carrying
   * the host's message; it does not suspend the workflow.
   *
   * Use `startDetached` when you need the run's id.
   */
  runDetached(name: string, input?: unknown): Promise<void>;
  /**
   * Start the workflow `name` detached and resolve with its run id -- a handle for polling, signalling, or recording somewhere
   * durable. As with `runDetached`, nothing here waits for that run, and its failure is not this workflow's.
   *
   * The id on replay is the recorded one, so a workflow that stores it stores the same id every time rather than a fresh run's.
   */
  startDetached(name: string, input?: unknown): Promise<string>;
}

export function makeDetached(h: Helpers): DetachedCtx {
  return {
    runDetached: (name, input) =>
      h.settle<null>(__cleat_run_detached(name, h.encode(input ?? null))).then(() => undefined),
    startDetached: (name, input) => h.settle<string>(__cleat_start_detached(name, h.encode(input ?? null))),
  };
}
