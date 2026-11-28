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

import { type UpdatesCtx, dispatchThen, makeUpdates } from "./groups/updates";
import { type SignalsCtx, makeSignals } from "./groups/signals";
import { type ChildrenCtx, makeChildren } from "./groups/children";
import { type DetachedCtx, makeDetached } from "./groups/detached";
import { type LocksCtx, makeLocks } from "./groups/locks";
import { type SchedulesCtx, makeSchedules } from "./groups/schedules";
import { type ContinuationCtx, makeContinuation } from "./groups/continuation";
import { type CallsCtx, makeCalls } from "./groups/calls";
import { type PluginsCtx, makePlugins } from "./groups/plugins";
import { type UtilitiesCtx, makeUtilities } from "./groups/utilities";
import { waitMs } from "./waits";
import type { Helpers } from "./groups/helpers";

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
declare function __cleat_side_effect_recorded(): string;
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
declare function __cleat_defer(description: string): string;
declare function __cleat_defer_phase(on: boolean): void;
declare function __cleat_drain_done(n: number): void;

// The drain that runs before a result is reported. Skipped while the segment is suspended: a suspended workflow has not
// exited, and its cleanup must not run (the plugin answers the host with the suspend sentinel instead).
async function finish(): Promise<void> {
  // Reachable in a SHIPPED build: a body that fires a suspending call without awaiting it returns normally while the
  // segment is already suspended (`ctx.sleep(...)` with no `await` -- `settle` hands the never-settling promise only to a
  // caller that awaits it), which is the case crates/cleat-macro/src/entry.rs documents for Rust.
  //
  // UNOBSERVABLE today, measured rather than assumed: removing this line leaves the test suite GREEN. Two reasons, and a
  // future reader needs both -- every effectful binding short-circuits on is_suspended(), and cleat_take_result refuses
  // a result written while suspended, so a drain that wrongly ran could leave no trace anywhere. Whoever makes this
  // observable (an effectful binding that does NOT short-circuit) should also make it testable.
  if (__cleat_suspended()) return;
  await drainDefers();
}

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

// The calls cleat#3827 adds live in groups/<group>.ts, one file per group, and arrive here as these interfaces.
export interface Ctx extends UpdatesCtx, SignalsCtx, ChildrenCtx, DetachedCtx, LocksCtx, SchedulesCtx, ContinuationCtx, CallsCtx, PluginsCtx, UtilitiesCtx {
  workflowId(): string;
  call<T = unknown>(service: string, operation: string, request?: unknown): Promise<T>;
  sleep(ms: number): Promise<void>;
  /**
   * Wait for a signal. `timeoutMs` defaults to 24 hours; pass `Infinity` to wait without a limit. `0` or less does NOT wait: it times
   * out at once. A positive fraction rounds up to a whole millisecond (cleat#4144).
   */
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
  /** `timeoutMs` defaults to 24 hours; pass `Infinity` to wait without a limit; `0` or less does not wait. A positive fraction rounds up (cleat#4144). */
  awaitPromise<T = unknown>(id: string, timeoutMs?: number): Promise<AwaitedPromise<T>>;
  /** Settle a promise with a value. Rejects if the id is unknown to the host. */
  resolvePromise(id: string, value: unknown): Promise<void>;
  /** Settle a promise with an error. Rejects if the id is unknown to the host. */
  rejectPromise(id: string, error: string): Promise<void>;
  /**
   * Register work to run when the workflow finishes — after it returns or throws, in REVERSE registration order, each body
   * awaited and a failing body not stopping the rest.
   *
   * It does NOT run when the workflow suspends: a suspended run has not finished, and running cleanup at the first sleep
   * would release locks a run that is about to continue still holds. It also does not run when the HOST kills the
   * workflow — this guest exports no drain for the host to call, so the deferred work is lost (cleat#3922, and stated in
   * DIVERGENCES.md).
   */
  defer(fn: () => unknown | Promise<unknown>, description?: string): Promise<void>;
}

// What a group module is handed instead of importing this file. `settle`, `encode` and `parseJSON` are function declarations, so
// they are available here although they are written above and below.
const helpers: Helpers = { settle, encode, parseJSON };

const ctx: Ctx = {
  workflowId: () => __cleat_workflow_id(),
  call: (service, operation, request) => settle(__cleat_call(service, operation, encode(request ?? null))).then(parseJSON) as Promise<never>,
  // `dispatchThen` delivers any pending update before the call (groups/updates.ts); with none pending it is the call, unchanged.
  sleep: (ms) => dispatchThen(() => settle<null>(__cleat_sleep(ms))).then(() => undefined),
  waitForSignal: (name, timeoutMs = 24 * 60 * 60 * 1000) =>
    dispatchThen(() => settle<{ name: string; payload: string; timedOut: boolean }>(__cleat_await_signals(JSON.stringify(Array.isArray(name) ? name : [name]), waitMs(timeoutMs)))).then((s) => ({
      name: s.name,
      payload: parseJSON(s.payload),
      timedOut: s.timedOut,
    })),
  child: <T>(name: string, input?: unknown) =>
    settle<string>(__cleat_child_workflow(name, encode(input ?? null))).then((runId) => dispatchThen(() => settle<string>(__cleat_await_child(runId))).then((r) => parseJSON(r) as T)),
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
  // cleat#4113: freeze a value. Ask the host first, SYNCHRONOUSLY (the order of host calls is part of the history), so `compute` does not run on a replay.
  sideEffect: <T>(compute: () => T) => {
    const raw = __cleat_side_effect_recorded();
    const looked = JSON.parse(raw) as Envelope;
    if (looked.suspended || looked.err !== undefined) return settle<T>(raw);
    if (looked.ok !== null && looked.ok !== undefined) return Promise.resolve(parseJSON((looked.ok as { recorded: string }).recorded) as T);
    return settle<string>(__cleat_side_effect(encode(compute()))).then((v) => parseJSON(v) as T);
  },
  setQueryState: (key, value) => __cleat_set_query_state(key, value),
  version: () => __cleat_version(),
  minVersion: () => __cleat_min_version(),
  runId: () => __cleat_run_id(),
  getScope: () => JSON.parse(__cleat_get_scope()) as Scope,
  setScope: (objectType, instanceKey) => settle<Scope>(__cleat_set_scope(objectType, instanceKey)),
  cancellation: () => JSON.parse(__cleat_poll_cancellation()) as Cancellation,
  createPromise: (name) => settle<string>(__cleat_create_promise(name)),
  awaitPromise: <T>(id: string, timeoutMs = 24 * 60 * 60 * 1000) =>
    dispatchThen(() => settle<{ value: string; timedOut: boolean }>(__cleat_await_promise(id, waitMs(timeoutMs)))).then((r) => ({
      value: parseJSON(r.value) as T,
      timedOut: r.timedOut,
    })),
  resolvePromise: (id, value) => settle<null>(__cleat_resolve_promise(id, encode(value))).then(() => undefined),
  rejectPromise: (id, error) => settle<null>(__cleat_reject_promise(id, error)).then(() => undefined),
  defer: (fn, description = "deferred work") =>
    settle<string>(__cleat_defer(description)).then((id) => {
      defers.push({ id: id, fn: fn });
    }),

  // ---- the groups (cleat#3827) ----
  ...makeUpdates(helpers),
  ...makeSignals(helpers),
  ...makeChildren(helpers),
  ...makeDetached(helpers),
  ...makeLocks(helpers),
  ...makeSchedules(helpers),
  ...makeContinuation(helpers),
  ...makeCalls(helpers),
  ...makePlugins(helpers),
  ...makeUtilities(helpers),
};

// ---- deferred work ------------------------------------------------------------------------------------------------------
// The bodies live here, in JavaScript, because that is where the closures are; the host's side is only the ID and the
// phase bracket. Drained in REVERSE order (Go's and Rust's defer order), each body awaited, and one failing body does not
// stop the rest — the same rules as cleat-sdk's run_deferred, which also DRAINS FIRST and runs after, so a body that
// suspends cannot leave the registry half run.
const defers: Array<{ id: string; fn: () => unknown | Promise<unknown> }> = [];

async function drainDefers(): Promise<number> {
  const taken = defers.splice(0, defers.length);
  let ran = 0;
  __cleat_defer_phase(true);
  try {
    for (let i = taken.length - 1; i >= 0; i--) {
      ran++;
      try {
        await taken[i].fn();
      } catch {
        // Isolated on purpose (cleat-sdk's run_deferred does the same): the remaining bodies still run.
      }
    }
  } finally {
    __cleat_defer_phase(false);
  }
  return ran;
}

// The host-driven drain (cleat#3826). A defer segment suspends at the first new call, and a suspended segment does not drain
// its own table (`finish` returns early), so the HOST calls the module's `__cleat_run_deferred` export, which is the plugin's
// `cleat_run_drain`. That evaluates a small module of its own which calls THIS, by way of `globalThis`: the registry lives in
// this module's scope, and Javy evaluates the bundle afresh on every call, so only the context's globals survive from the
// entry point's call to the host's. Registered by `entry` below -- the evaluation that ran the workflow is the one whose
// `defers` the bodies were pushed to. It reports how many bodies ran, and never a result: the outcome was already decided.
function registerDrain(): void {
  (globalThis as { __cleat_drain?: () => void }).__cleat_drain = () => {
    drainDefers().then(__cleat_drain_done, () => __cleat_drain_done(0));
  };
}

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
    // A module instance can serve more than one call, and a body that re-runs on replay re-registers everything it had:
    // without this the registry would accumulate a duplicate per segment.
    defers.length = 0;
    registerDrain();
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
        async (v) => {
          await finish();
          __cleat_result(resultText(v === undefined ? {} : v));
        },
        async (e) => {
          if (e instanceof SuspendSignal) return; // the throw-mode control only: a suspension that surfaced as an exception
          await finish();
          __cleat_fail(e instanceof Error ? `${e.name}: ${e.message}${e.stack ? "\n" + mapStack(e.stack) : ""}` : String(e));
        },
      )
      // An exception in the success handler above is NOT caught by the failure handler beside it, and would be dropped: the plugin
      // would then report only "the workflow returned no result" and the cause (a result that will not serialise, say) is lost.
      .catch((e) => __cleat_fail(`could not return the result: ${e instanceof Error ? e.message : String(e)}`));
  };
}
