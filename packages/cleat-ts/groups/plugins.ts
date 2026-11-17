// The `plugins` group of host-call bindings (cleat#3827 group I).
//
// Three calls that share one registry: the plugin's `plugin_call`, `plugin_call_streaming`, and the engine's
// `cleat_wait_for_event` -- the last because its claim loop calls the plugin path on every attempt, so it is scripted
// through the same registry. Conventions: groups/README.md.

import type { Helpers } from "./helpers";

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

export interface PluginsCtx {
  /**
   * Call a named plugin function and resolve with its JSON answer.
   *
   * On replay the recorded answer is returned and the plugin is not called again, except where the function was
   * registered with a policy that permits re-invocation.
   */
  pluginCall<T = unknown>(plugin: string, func: string, input?: unknown): Promise<T>;

  /**
   * The streaming form of {@link pluginCall}. Resolves with the streamed answer; the host assembles the chunks.
   *
   * On replay, as `pluginCall`: the recorded answer is used.
   */
  pluginCallStreaming<T = unknown>(plugin: string, func: string, input?: unknown): Promise<T>;

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
      h.settle(__cleat_plugin_call_streaming(plugin, func, h.encode(input ?? null))).then(h.parseJSON) as Promise<never>,

    waitForEvent: (plugin, func, signalNames, opts) =>
      h
        .settle(
          __cleat_wait_for_event(
            plugin,
            func,
            h.encode(opts?.input ?? null),
            JSON.stringify(Array.isArray(signalNames) ? signalNames : [signalNames]),
            opts?.timeoutMs ?? DEFAULT_EVENT_TIMEOUT_MS,
          ),
        )
        .then(h.parseJSON) as Promise<never>,
  };
}
