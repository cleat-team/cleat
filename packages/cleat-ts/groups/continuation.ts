// The `continuation` group of host-call bindings (cleat#3827): `continueAsNew`, which ends this run and starts a fresh one with a
// new input (and, optionally, a new workflow version). The group is `continuation` and not `continue`, a Rust keyword.
//
// NOT A DISPATCH POINT: neither the Go nor the Rust SDK dispatches updates before continuing as new, so this does not either.

import type { Helpers } from "./helpers";

declare function __cleat_continue_as_new(inputJson: string): string;
declare function __cleat_continue_as_new_versioned(inputJson: string, version: number): string;

export interface ContinueAsNewOptions {
  /** Start the new run on this workflow version (a whole number from 1). Omitted means the current version. */
  version?: number;
}

export interface ContinuationCtx {
  /**
   * End this run and start a new one with `input`. The returned promise NEVER SETTLES when the host accepts the call, so the code
   * after `await ctx.continueAsNew(...)` is unreachable, as after a `return`.
   *
   * That is deliberate and it is not what the host does: the engine records the continuation and returns success to the guest, which
   * would otherwise run on and record events for a run that has already been replaced. Rejects with a `CleatError` (and so does continue
   * running) only when the host refuses: a malformed input, an invalid `options.version`, or a defer phase.
   *
   * **`options.version` IS NOT HONOURED BY A WORKER.** The engine records the requested version on the event, but `cleat-worker`'s
   * continue-as-new path starts the new run on the SAME version it was on (`cmd/cleat-worker/setup.go` passes `wf.DefVersion` and never
   * reads the suspension's `NewVersion`), so on a worker the versioned form behaves as the plain one. This is cleat#4041, the
   * engine's and shared by every SDK, and `TestTypeScriptContinueAsNewOnARealWorker` pins it on all three dialects.
   *
   * On replay the recorded continuation is returned to the same suspension, and no second event is recorded. An update that arrives after
   * the run's last dispatch point is not delivered before it is replaced.
   */
  continueAsNew(input: unknown, options?: ContinueAsNewOptions): Promise<never>;
}

export function makeContinuation(h: Helpers): ContinuationCtx {
  return {
    continueAsNew: async (input, options) => {
      const json = h.encode(input);
      const raw =
        options?.version === undefined ? __cleat_continue_as_new(json) : __cleat_continue_as_new_versioned(json, options.version);
      await h.settle<null>(raw);
      // Accepted: the run is over, whatever the guest does next. Never settle, so nothing after the call runs.
      return new Promise<never>(() => {});
    },
  };
}
