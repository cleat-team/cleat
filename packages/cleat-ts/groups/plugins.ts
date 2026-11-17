// The `plugins` group of host-call bindings (cleat#3827 group I).
//
// Three calls that share one registry: the plugin's `plugin_call`, `plugin_call_streaming`, and the engine's
// `cleat_wait_for_event` -- the last because its claim loop calls the plugin path on every attempt, so it is scripted
// through the same registry. Conventions: groups/README.md.

import type { Helpers } from "./helpers";
import { dispatchThen } from "./updates";

declare function __cleat_plugin_call(plugin: string, func: string, input: string): string;
declare function __cleat_plugin_call_streaming(plugin: string, func: string, input: string): string;
declare function __cleat_wait_for_event(
  plugin: string,
  func: string,
  input: string,
  namesJson: string,
  timeoutMs: number,
): string;

export interface WaitForEventOptions {
  /** The claim-shaped function's input. Omitted means `null`, as `call` does. */
  input?: unknown;
  /** How long the host keeps registering and re-claiming. Defaults to 24 hours, as `waitForSignal` does. */
  timeoutMs?: number;
}

/** One chunk of a streamed plugin answer, as the host emits it. */
export interface StreamChunk {
  /** Position in the stream, from 0. */
  i: number;
  /** The chunk's content. */
  c: string;
  /** Whether this is the last chunk. */
  f: boolean;
}

export interface PluginsCtx {
  /**
   * Call a named plugin function and resolve with its JSON answer.
   *
   * On replay the recorded answer is returned and the plugin is not called again, except where the function was
   * registered with a policy that permits re-invocation.
   */
  pluginCall<T = unknown>(plugin: string, func: string, input?: unknown): Promise<T>;

  /**
   * The streaming form of {@link pluginCall}.
   *
   * Resolves with the CHUNK ARRAY as the host emits it — `[{ i: 0, c: "a", f: false }, …]` — not a joined string. `i` is
   * the chunk's index, `c` its content, `f` whether it is the last one. Reading it as a joined answer is the mistake this
   * signature exists to make impossible: an earlier draft of the docs said the host assembled the chunks, and it does not.
   *
   * On replay, as `pluginCall`: the recorded chunks are used.
   */
  pluginCallStreaming(plugin: string, func: string, input?: unknown): Promise<StreamChunk[]>;

  /**
   * Wait for an external event, claimed by `plugin.func` and woken by any of `signalNames`.
   *
   * The function must be CLAIM-SHAPED: its JSON answer carries a boolean `found`. `true` returns that answer; `false`
   * means the host registers the workflow as an awaiter and waits. An answer with no boolean `found` is reported as a
   * failure rather than waited on, so a function that never answers the question cannot burn the whole budget.
   *
   * A TIMEOUT REJECTS, with the host's message. That differs from `waitForSignal`, which resolves with
   * `{timedOut: true}`: the host reports this one as a call failure, so a `catch` is what distinguishes "no event"
   * from "an event".
   *
   * That is not a divergence between languages, and it was checked rather than assumed: AssemblyScript's
   * `waitForEventMs` reports the same timeout the same way (its `errCode` path sets `error`), and Go's
   * `PluginCallTyped` returns `(Resp, error)`. The two host calls simply answer a timeout differently, and every SDK
   * mirrors its own export. What follows is that `waitForEvent` and `waitForSignal` cannot be caught by one shape.
   *
   * On replay every claim attempt is recorded, so the same claims are answered and the wait does not re-poll live.
   */
  waitForEvent<T = unknown>(
    plugin: string,
    func: string,
    signalNames: string | string[],
    opts?: WaitForEventOptions,
  ): Promise<T>;
}

// The host keeps re-claiming rather than polling at a fixed rate, so this is the caller's total budget, not one
// attempt's wait. Matches waitForSignal's default.
const DEFAULT_EVENT_TIMEOUT_MS = 24 * 60 * 60 * 1000;

export function makePlugins(h: Helpers): PluginsCtx {
  return {
    pluginCall: (plugin, func, input) =>
      h.settle(__cleat_plugin_call(plugin, func, h.encode(input ?? null))).then(h.parseJSON) as Promise<never>,

    pluginCallStreaming: (plugin, func, input) =>
      h
        .settle<StreamChunk[]>(__cleat_plugin_call_streaming(plugin, func, h.encode(input ?? null)))
        .then(h.parseJSON) as Promise<StreamChunk[]>,

    // WRAPPED IN dispatchThen, as `sleep`, `waitForSignal`, the wait inside `child` and `awaitPromise` all are. A wait is
    // one of the fixed program positions an update can be delivered at (groups/updates.ts): without the wrapper a workflow
    // blocked here answers "no update handler registered" for an update that was waiting the whole time. It is also the
    // position with the most to lose from missing it -- nothing else is going to ask while the workflow sits in a wait.
    waitForEvent: (plugin, func, signalNames, opts) => {
      // A budget the host cannot honour is refused here rather than passed through: NaN becomes 0 in the ABI's f64 ->
      // i64 conversion and a negative value makes the first `remaining <= 0` test fire, so both silently mean "no wait at
      // all" -- a timeout reported for a workflow that asked to wait.
      const timeoutMs = opts?.timeoutMs ?? DEFAULT_EVENT_TIMEOUT_MS;
      if (!Number.isFinite(timeoutMs) || timeoutMs < 0) {
        return Promise.reject(
          new Error(`waitForEvent: timeoutMs must be a finite, non-negative number, got ${String(opts?.timeoutMs)}`),
        );
      }
      return dispatchThen(() =>
        h
          .settle(
            __cleat_wait_for_event(
              plugin,
              func,
              h.encode(opts?.input ?? null),
              JSON.stringify(Array.isArray(signalNames) ? signalNames : [signalNames]),
              timeoutMs,
            ),
          )
          .then(h.parseJSON),
      ) as Promise<never>;
    },
  };
}
