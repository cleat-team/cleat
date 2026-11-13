// cleat-ts (prototype, branch proto/ts-javy): the TypeScript layer over the globals the cleat-js plugin installs.
// Plan section 4.1/4.4. Nothing here is supported or claimed.
//
// SUSPENSION: a host call that suspends the workflow returns {"suspended":true}; the promise this layer hands back for it
// NEVER SETTLES (it is a real Promise with no resolver anyone holds). Nothing is thrown, so a user's try/catch cannot
// swallow a suspension and a `finally` after an `await` of it never runs: the awaiting async function is simply never
// resumed. The plugin returns the suspend sentinel when the entry's microtasks have drained and its flag is set.
//
// Build-time switch for the red control (plan 4.4): `SUSPEND_MODE === "throw"` makes a suspended call THROW instead, the
// "throw Suspend" design the plan rejects. It exists only so a test can show a `finally` running under it.

declare const SUSPEND_MODE: string | undefined;
// Build-time switch for the Q4 red control: RESULT_CANONICAL=false sends the result as raw JSON.stringify, so a test can show a
// multi-key result is NOT byte-identical to its Go twin without the host's canonical form (ABI 6.3, "SHOULD").
declare const RESULT_CANONICAL: boolean | undefined;

declare function __cleat_args(): string;
declare function __cleat_result(json: string): void;
declare function __cleat_fail(message: string): void;
declare function __cleat_suspended(): boolean;
declare function __cleat_call(service: string, operation: string, request: string): string;
declare function __cleat_sleep(ms: number): string;
declare function __cleat_await_signals(namesJson: string, timeoutMs: number): string;
declare function __cleat_child_workflow(name: string, input: string): string;
declare function __cleat_await_child(runId: string): string;
declare function __cleat_now(): number;
declare function __cleat_random(): string;
declare function __cleat_uuid(seed: string): string;
declare function __cleat_log(message: string): void;
declare function __cleat_json_stringify(text: string): string;
declare function __cleat_workflow_id(): string;
declare function __cleat_set_query_state(key: string, value: string): void;
declare function __cleat_side_effect(computed: string): string;

export class CleatError extends Error {}
class SuspendSignal extends Error {}

type Envelope = { ok?: unknown; err?: string; suspended?: true };

function settle<T>(raw: string): Promise<T> {
  const r = JSON.parse(raw) as Envelope;
  if (r.suspended) {
    if (typeof SUSPEND_MODE !== "undefined" && SUSPEND_MODE === "throw") return Promise.reject(new SuspendSignal("suspended"));
    return new Promise<T>(() => {}); // never settles
  }
  if (r.err !== undefined) return Promise.reject(new CleatError(r.err));
  return Promise.resolve(r.ok as T);
}

function parseJSON(s: unknown): unknown {
  if (typeof s !== "string" || s === "") return s;
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
}

export interface SignalResult {
  name: string;
  payload: unknown;
  timedOut: boolean;
}

export interface Ctx {
  workflowId(): string;
  call<T = unknown>(service: string, operation: string, request?: unknown): Promise<T>;
  sleep(ms: number): Promise<void>;
  waitForSignal(name: string | string[], timeoutMs?: number): Promise<SignalResult>;
  /** Start a child workflow and wait for its result. */
  child<T = unknown>(name: string, input?: unknown): Promise<T>;
  now(): number;
  random(): string;
  uuid(seed?: string): string;
  /** Like `call`, but the request is already JSON TEXT (e.g. from `canonical`): sent as is, not stringified again. */
  callRaw(service: string, operation: string, requestJson: string): Promise<unknown>;
  /** DurableLog. */
  log(message: string): void;
  /** The host's canonical form of a JSON text (ABI 6.3): sorted keys, Go encoding/json byte form. Throws if the host refuses it. */
  canonical(jsonText: string): string;
  sideEffect<T>(compute: () => T): Promise<T>;
  setQueryState(key: string, value: string): void;
}

const ctx: Ctx = {
  workflowId: () => __cleat_workflow_id(),
  call: (service, operation, request) => settle(__cleat_call(service, operation, JSON.stringify(request ?? null))).then(parseJSON) as Promise<never>,
  sleep: (ms) => settle<null>(__cleat_sleep(ms)).then(() => undefined),
  waitForSignal: (name, timeoutMs = 24 * 60 * 60 * 1000) =>
    settle<{ name: string; payload: string; timedOut: boolean }>(__cleat_await_signals(JSON.stringify(Array.isArray(name) ? name : [name]), timeoutMs)).then((s) => ({
      name: s.name,
      payload: parseJSON(s.payload),
      timedOut: s.timedOut,
    })),
  child: <T>(name: string, input?: unknown) =>
    settle<string>(__cleat_child_workflow(name, JSON.stringify(input ?? null))).then((runId) => settle<string>(__cleat_await_child(runId)).then((r) => parseJSON(r) as T)),
  now: () => __cleat_now(),
  random: () => __cleat_random(),
  uuid: (seed = "") => __cleat_uuid(seed),
  callRaw: (service, operation, requestJson) => settle(__cleat_call(service, operation, requestJson)).then(parseJSON) as Promise<never>,
  log: (message) => __cleat_log(message),
  canonical: (jsonText) => {
    const out = __cleat_json_stringify(jsonText);
    if (out === "") throw new Error("the host refused to canonicalise: " + jsonText.slice(0, 80));
    return out;
  },
  sideEffect: <T>(compute: () => T) => settle<string>(__cleat_side_effect(JSON.stringify(compute()))).then((v) => parseJSON(v) as T),
  setQueryState: (key, value) => __cleat_set_query_state(key, value),
};

/**
 * Wraps an async workflow function as the zero-argument function Javy exports. It reads the ABI input, runs `fn`, and
 * reports the outcome through the plugin: a value becomes the JSON result, a rejection or a throw becomes an error
 * result, and a promise that never settles (a suspension) reports nothing, which the plugin turns into the sentinel.
 */
// The workflow's result as text. Canonical by default: parsed and re-encoded by the host (Go encoding/json), so a TS result is
// byte-identical to what a Go or Rust workflow returning the same value would produce. If the host refuses (it cannot, for text
// JSON.stringify produced) the raw text is sent rather than losing the result.
function resultText(v: unknown): string {
  const raw = JSON.stringify(v);
  if (typeof RESULT_CANONICAL !== "undefined" && RESULT_CANONICAL === false) return raw;
  const canon = __cleat_json_stringify(raw);
  return canon === "" ? raw : canon;
}

export function workflow<I = unknown, O = unknown>(fn: (ctx: Ctx, input: I) => O | Promise<O>): () => void {
  return function entry(): void {
    let input: I;
    try {
      input = JSON.parse(__cleat_args() || "null") as I;
    } catch (e) {
      __cleat_fail(`the workflow input is not valid JSON: ${(e as Error).message}`);
      return;
    }
    Promise.resolve()
      .then(() => fn(ctx, input))
      .then(
        (v) => __cleat_result(resultText(v === undefined ? {} : v)),
        (e) => {
          if (e instanceof SuspendSignal) return; // the throw-mode control only: a suspension that surfaced as an exception
          __cleat_fail(e instanceof Error ? `${e.name}: ${e.message}${e.stack ? "\n" + e.stack : ""}` : String(e));
        },
      )
      // An exception in the success handler above is NOT caught by the failure handler beside it, and would be dropped: the plugin
      // would then report only "the workflow returned no result" and the cause (a result that will not serialise, say) is lost.
      .catch((e) => __cleat_fail(`could not return the result: ${e instanceof Error ? e.message : String(e)}`));
  };
}
