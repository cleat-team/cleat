// cleat-ts (in development, not supported: see tiers.yaml, cleat#3825): the TypeScript layer over the globals the cleat-js plugin installs.
// Plan section 4.1/4.4. Nothing here is supported or claimed.
//
// SUSPENSION: a host call that suspends the workflow returns {"suspended":true}; the promise this layer hands back for it
// NEVER SETTLES (it is a real Promise with no resolver anyone holds). Nothing is thrown, so a user's try/catch cannot
// swallow a suspension and a `finally` after an `await` of it never runs: the awaiting async function is simply never
// resumed. The plugin returns the suspend sentinel when the entry's microtasks have drained and its flag is set.
//
// Build-time switch for the red control: `SUSPEND_MODE === "throw"` makes a suspended call THROW instead, the
// "throw Suspend" design the plan rejects. It exists only so a test can show a `finally` running under it.

declare const SUSPEND_MODE: string | undefined;
// Build-time switch for the Q4 red control: RESULT_CANONICAL=false sends the result as raw JSON.stringify, so a test can show a
// multi-key result is NOT byte-identical to its Go twin without the host's canonical form (ABI 6.3, "SHOULD").
declare const RESULT_CANONICAL: boolean | undefined;
// Build-time switch for the NaN red control: NAN_POLICY="lenient" restores plain JSON.stringify, which turns NaN and +-Infinity into
// null. The default REFUSES them with the text Go's encoding/json uses, so a TS workflow and its Go twin fail the same way.
declare const NAN_POLICY: string | undefined;

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
declare function __cleat_version(): number;
declare function __cleat_min_version(): number;
declare function __cleat_run_id(): string;
declare function __cleat_poll_cancellation(): string;
declare function __cleat_get_scope(): string;
declare function __cleat_set_scope(objectType: string, instanceKey: string): string;
declare function __cleat_create_promise(name: string): string;
declare function __cleat_await_promise(id: string, timeoutMs: number): string;
declare function __cleat_resolve_promise(id: string, value: string): string;
declare function __cleat_reject_promise(id: string, error: string): string;

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

/** A virtual-object scope: what a nested call's durable state is keyed by. Empty strings mean no scope is set. */
export interface Scope {
  objectType: string;
  instanceKey: string;
}

/** The host's answer to "has this run been asked to cancel", and its reason when it has. */
export interface Cancellation {
  cancelled: boolean;
  reason: string;
}

/** The outcome of waiting for a promise. `timedOut` true means the wait expired, not that anything failed. */
export interface AwaitedPromise<T> {
  value: T;
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
  /** The run's workflow definition version (Go's Version()). */
  version(): number;
  /** The oldest version a worker may run this under (Go's MinVersion()). */
  minVersion(): number;
  /**
   * The current run's id. The engine currently sets both this and `workflowId()` from the same value
   * (`engine/executor.go`), so they agree today; they are separate ABI fields because a run id identifies ONE execution
   * and a workflow id survives continue-as-new.
   */
  runId(): string;
  /** The scope this run acts under, empty strings when none is set. */
  getScope(): Scope;
  /**
   * Set the virtual-object scope for subsequent durable state, and resolve to the PREVIOUS one (both fields empty when
   * none was set). An empty type and key clears the scope.
   *
   * RESOLUTION IS NOT "I HOLD THE SCOPE". When another run holds the key, the host records the refusal and STOPS THE RUN
   * AT THE END OF THE SEGMENT, but the call itself answers success (cleat#3916) — so code between this call and the end
   * of the segment runs without the key. Await it, and do not treat it as a lock acquisition.
   */
  setScope(objectType: string, instanceKey: string): Promise<Scope>;
  /**
   * A poll: whether this run has been asked to cancel, and the reason when it has. Nothing is waited for.
   *
   * The host answers `false` on every REPLAYED segment (`engine/signaller.go`), so this is a constant per mode rather
   * than a function of recorded state (cleat#3917): code that branches on it can take a different path on replay. Read
   * it, do not branch on it in a way that changes what you do.
   */
  cancellation(): Cancellation;
  /**
   * Create a durable promise and resolve to its id. Knowing the id is the authority to settle it, from any run: that is
   * how a request/reply pair is built (create + signal the id + await here + resolve there).
   */
  createPromise(name: string): Promise<string>;
  /** Wait for a promise. A rejection is a `CleatError`; an expired timeout resolves with `timedOut: true`. */
  awaitPromise<T = unknown>(id: string, timeoutMs?: number): Promise<AwaitedPromise<T>>;
  /** Settle a promise with a value. Rejects if the id is unknown to the host. */
  resolvePromise(id: string, value: unknown): Promise<void>;
  /** Settle a promise with an error. Rejects if the id is unknown to the host. */
  rejectPromise(id: string, error: string): Promise<void>;
}

const ctx: Ctx = {
  workflowId: () => __cleat_workflow_id(),
  call: (service, operation, request) => settle(__cleat_call(service, operation, encode(request ?? null))).then(parseJSON) as Promise<never>,
  sleep: (ms) => settle<null>(__cleat_sleep(ms)).then(() => undefined),
  waitForSignal: (name, timeoutMs = 24 * 60 * 60 * 1000) =>
    settle<{ name: string; payload: string; timedOut: boolean }>(__cleat_await_signals(JSON.stringify(Array.isArray(name) ? name : [name]), timeoutMs)).then((s) => ({
      name: s.name,
      payload: parseJSON(s.payload),
      timedOut: s.timedOut,
    })),
  child: <T>(name: string, input?: unknown) =>
    settle<string>(__cleat_child_workflow(name, encode(input ?? null))).then((runId) => settle<string>(__cleat_await_child(runId)).then((r) => parseJSON(r) as T)),
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
  sideEffect: <T>(compute: () => T) => settle<string>(__cleat_side_effect(encode(compute()))).then((v) => parseJSON(v) as T),
  setQueryState: (key, value) => __cleat_set_query_state(key, value),
  version: () => __cleat_version(),
  minVersion: () => __cleat_min_version(),
  runId: () => __cleat_run_id(),
  getScope: () => JSON.parse(__cleat_get_scope()) as Scope,
  setScope: (objectType, instanceKey) => settle<Scope>(__cleat_set_scope(objectType, instanceKey)),
  cancellation: () => JSON.parse(__cleat_poll_cancellation()) as Cancellation,
  createPromise: (name) => settle<string>(__cleat_create_promise(name)),
  awaitPromise: <T>(id: string, timeoutMs = 24 * 60 * 60 * 1000) =>
    settle<{ value: string; timedOut: boolean }>(__cleat_await_promise(id, timeoutMs)).then((r) => ({
      value: parseJSON(r.value) as T,
      timedOut: r.timedOut,
    })),
  resolvePromise: (id, value) => settle<null>(__cleat_resolve_promise(id, encode(value))).then(() => undefined),
  rejectPromise: (id, error) => settle<null>(__cleat_reject_promise(id, error)).then(() => undefined),
};

/**
 * Wraps an async workflow function as the zero-argument function Javy exports. It reads the ABI input, runs `fn`, and
 * reports the outcome through the plugin: a value becomes the JSON result, a rejection or a throw becomes an error
 * result, and a promise that never settles (a suspension) reports nothing, which the plugin turns into the sentinel.
 */
// Every value that leaves the guest (a call's request, a child's input, a side effect, the result) is encoded here. A silent null is a
// data change; a failure is visible, and it is what the Go SDK does (json: unsupported value: NaN / +Inf / -Inf). Measured in xlang/.
function encode(v: unknown): string {
  if (typeof NAN_POLICY !== "undefined" && NAN_POLICY === "lenient") return JSON.stringify(v);
  return JSON.stringify(v, (_k, x) => {
    if (typeof x === "number" && !Number.isFinite(x)) throw new Error(`json: unsupported value: ${Number.isNaN(x) ? "NaN" : x > 0 ? "+Inf" : "-Inf"}`);
    return x;
  });
}

// The workflow's result as text. Canonical by default: parsed and re-encoded by the host (Go encoding/json), so a TS result is
// byte-identical to what a Go or Rust workflow returning the same value would produce. If the host refuses (it cannot, for text
// JSON.stringify produced) the raw text is sent rather than losing the result.
function resultText(v: unknown): string {
  const raw = encode(v);
  if (typeof RESULT_CANONICAL !== "undefined" && RESULT_CANONICAL === false) return raw;
  const canon = __cleat_json_stringify(raw);
  return canon === "" ? raw : canon;
}

// ---- source maps -------------------------------------------------------------------------------------------------------
// The build appends `globalThis.__cleat_sm = <JSON>` to the bundle (build.sh): {"s": [source paths], "m": esbuild's mappings string}.
// QuickJS reports a frame as `function.mjs:LINE:COL`, a position in the BUNDLE; this rewrites each such frame to the original
// `src.ts:LINE:COL`. Decoded lazily, once, at the first error, so a workflow that never fails pays only for the string in its heap.
// Absent (SOURCEMAP=0 builds) or unparsable: the stack is returned untouched, never replaced by a guess.
const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
let SM: { s: string[]; lines: number[][][] } | null | undefined;
function loadMap(): { s: string[]; lines: number[][][] } | null {
  if (SM !== undefined) return SM;
  SM = null;
  try {
    const raw = (globalThis as { __cleat_sm?: string }).__cleat_sm;
    if (!raw) return SM;
    const j = JSON.parse(raw) as { s: string[]; m: string };
    const lines: number[][][] = [];
    let sIdx = 0, sLine = 0, sCol = 0;
    for (const line of j.m.split(";")) {
      const segs: number[][] = [];
      let gCol = 0;
      if (line) {
        for (const seg of line.split(",")) {
          const f: number[] = [];
          let shift = 0, val = 0;
          for (let i = 0; i < seg.length; i++) {
            const d = B64.indexOf(seg[i]);
            val += (d & 31) << shift;
            if (d & 32) shift += 5;
            else { f.push(val & 1 ? -(val >>> 1) : val >>> 1); val = 0; shift = 0; }
          }
          gCol += f[0];
          if (f.length >= 4) { sIdx += f[1]; sLine += f[2]; sCol += f[3]; segs.push([gCol, sIdx, sLine, sCol]); }
        }
      }
      lines.push(segs);
    }
    SM = { s: j.s, lines };
  } catch { SM = null; }
  return SM;
}
function mapStack(stack: string): string {
  const m = loadMap();
  if (!m) return stack;
  return stack.replace(/function\.mjs:(\d+):(\d+)/g, (whole, l: string, c: string) => {
    const segs = m.lines[Number(l) - 1];
    if (!segs) return whole;
    const col0 = Number(c) - 1; // QuickJS columns are 1-based, the map's are 0-based
    let best: number[] | undefined;
    for (const sg of segs) { if (sg[0] <= col0) best = sg; else break; }
    return best ? `${m.s[best[1]]}:${best[2] + 1}:${best[3] + 1}` : whole;
  });
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
          __cleat_fail(e instanceof Error ? `${e.name}: ${e.message}${e.stack ? "\n" + mapStack(e.stack) : ""}` : String(e));
        },
      )
      // An exception in the success handler above is NOT caught by the failure handler beside it, and would be dropped: the plugin
      // would then report only "the workflow returned no result" and the cause (a result that will not serialise, say) is lost.
      .catch((e) => __cleat_fail(`could not return the result: ${e instanceof Error ? e.message : String(e)}`));
  };
}
