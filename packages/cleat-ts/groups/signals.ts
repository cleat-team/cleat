// The `signals` group of host-call bindings (cleat#3827): `pollSignal` and `signalWorkflow`. `waitForSignal`, which parks the
// workflow until a signal arrives, was already bound in index.ts and is not part of this group.
//
// NEITHER CALL IS A DISPATCH POINT. Neither can park the workflow, so neither goes through `dispatchThen` (the update group's
// hook, which runs before a call that can suspend): `pollSignal` answers at once and `signalWorkflow` is fire-and-forget.

import type { Helpers } from "./helpers";

declare function __cleat_poll_signal(name: string): string;
declare function __cleat_signal_workflow(targetRunId: string, name: string, payloadJson: string): string;

export interface SignalsCtx {
  /**
   * Look for a delivered signal called `name` WITHOUT waiting: resolves with its payload (parsed as JSON when it is JSON,
   * otherwise the string as sent), or `undefined` when none has arrived. Use `waitForSignal` to wait for one.
   *
   * It returns a promise even though nothing is waited for, so that a name the host refuses is a `CleatError` like every other
   * call's failure; it does not suspend the workflow and the promise settles at once.
   *
   * IT DOES NOT CONSUME THE SIGNAL. The engine records no event for a poll and removes nothing from the queue, so the same
   * signal is returned again by the next poll, and is the one a following `waitForSignal` receives and consumes. A loop that
   * polls until `undefined` therefore never ends.
   *
   * On replay the answer depends on the delivery time against the run's recorded clock and not on recorded history
   * (`engine/signaller.go`), so it is the same on every replay; but because nothing is recorded, code that branches on it is
   * replayed against the store as it is then, not as it was.
   */
  pollSignal(name: string): Promise<unknown | undefined>;
  /**
   * Send a signal to the run `targetRunId`, fire-and-forget: it resolves when the host has taken it and does not wait for the
   * target to receive it. `payload` is JSON-encoded (an omitted payload is sent as `null`), so a string round-trips as the same
   * string through the target's `waitForSignal`.
   *
   * Recorded as a `signal_received` event, which on replay is matched and NOT delivered a second time. Refused in a defer
   * phase, which exists to clean up and not to start work.
   */
  signalWorkflow(targetRunId: string, name: string, payload?: unknown): Promise<void>;
}

export function makeSignals(h: Helpers): SignalsCtx {
  return {
    pollSignal: (name) =>
      h.settle<{ payload: string } | null>(__cleat_poll_signal(name)).then((r) => (r === null ? undefined : h.parseJSON(r.payload))),
    signalWorkflow: (targetRunId, name, payload) =>
      h.settle<null>(__cleat_signal_workflow(targetRunId, name, h.encode(payload === undefined ? null : payload))).then(() => undefined),
  };
}
