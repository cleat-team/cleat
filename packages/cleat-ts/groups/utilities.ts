// The `utilities` group of host-call bindings (cleat#3827 group J): `cleat_fetch` and `cleat_json_parse`.
//
// Both are stateless request/response and neither suspends, so both are SYNCHRONOUS (README rule 2) -- they do not go
// through `h.settle`, which exists for the calls that answer with a suspension. They answer with the same
// `{"ok"|"err"}` envelope the other bindings use, unwrapped here.
//
// `fetch` THROWS on a refusal -- an egress policy, or an engine with no Fetcher at all -- because the host reports those
// as a call error and not as a status. A status is something a server chose to answer, and confusing the two is how a
// refused request reads as a 403.

import type { Helpers } from "./helpers";

declare function __cleat_fetch(method: string, url: string, headers: string, body: string): string;
declare function __cleat_json_parse(json: string): string;

/** The host's answer to a fetch, as the SDK decodes it. */
export interface FetchResult {
  status: number;
  headers: Record<string, string>;
  body: string;
}

export interface FetchOptions {
  method?: string;
  headers?: Record<string, string>;
  /** Sent as-is; the caller encodes it if it is not already text. */
  body?: string;
}

export interface UtilitiesCtx {
  /**
   * Perform an HTTP request through the host's fetcher and return its answer.
   *
   * THROWS when the request is refused rather than answered: no Fetcher on this engine, an egress policy that excludes
   * the URL, or a transport failure. A refused request has no status, and reporting one would make "the policy said no"
   * indistinguishable from "the server said 403".
   *
   * On replay the recorded answer is returned and no request is made.
   */
  fetch(url: string, opts?: FetchOptions): FetchResult;

  /**
   * Parse JSON text and return it as a value, via the host's canonical form: sorted keys, Go `encoding/json` byte form
   * (ABI 6.3).
   *
   * THROWS if the host refuses the text as JSON. `canonical()` is a different call -- it returns that canonical TEXT --
   * and this one returns the parsed value.
   *
   * On replay nothing is recorded: it is a pure function of its argument.
   */
  jsonParse<T = unknown>(jsonText: string): T;
}

/**
 * Unwrap a synchronous binding's envelope. Not `h.settle`, which returns a promise: these bindings never suspend, so a
 * suspension here would be a bug in the binding rather than a state of the workflow, and it is reported as one.
 */
function unwrap(raw: string): string {
  const env = JSON.parse(raw) as { ok?: unknown; err?: string; suspended?: boolean };
  if (env.suspended) {
    throw new Error("cleat: a utilities call suspended, which it cannot do; the binding is wrong, not the workflow");
  }
  if (typeof env.err === "string") throw new Error(env.err);
  return typeof env.ok === "string" ? env.ok : JSON.stringify(env.ok ?? null);
}

export function makeUtilities(h: Helpers): UtilitiesCtx {
  return {
    fetch: (url, opts) => {
      const answer = unwrap(
        __cleat_fetch(opts?.method ?? "GET", url, h.encode(opts?.headers ?? {}), opts?.body ?? ""),
      );
      return h.parseJSON(answer) as FetchResult;
    },

    jsonParse: <T,>(jsonText: string): T => h.parseJSON(unwrap(__cleat_json_parse(jsonText))) as T,
  };
}
