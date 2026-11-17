// The `children` group of host-call bindings (cleat#3827): start a child with per-child options, wait for several at once, and
// read one child's state without waiting.
//
// WHAT THE HOST ACTUALLY ANSWERS, because the three calls do not agree with each other and the shapes below are derived from
// `engine/children.go` rather than from what a reader would guess:
//
//   await_all_children  answers an ARRAY in the order the run ids were passed, one entry per id:
//                       {"run_id":"...","result":"<json text>"} or {"run_id":"...","error":"..."}.
//   await_any_child     answers ONE entry of that same shape, after SORTING the run ids, so when several children are already
//                       finished the winner is the lowest-sorting id rather than the first one the caller named.
//   poll_child          answers {"status":"running"|"completed"|"failed","result":"<json text>","error":"..."}, and a store that
//                       could not answer is a HOST error (the ABI fails the call) rather than a failed child (cleat#3387).
//
// `result` is JSON TEXT on the wire even here, because the engine marshals the child's own result as a string; it is parsed with
// `parseJSON` exactly as `Ctx.child` does, so a child returning a string and a child returning nothing stay distinguishable.
//
// REPLAY. Every call here is recorded (poll is not, and needs not to be: it answers against the parent's durable clock), so they
// replay from history like any other durable call. Two consequences a workflow can observe: `awaitAny`'s winner is fixed by the
// recorded answer even if the children could finish in a different order now, and `pollChild` answers as of the parent's durable
// time rather than as of the moment it is called.
//
// THE DISPATCH IS index.ts's JOB, and these two waits go through it because they can block for as long as it takes other runs to
// finish -- the same points at which the Rust and Go SDKs dispatch pending updates. `pollChild` does not: it never waits.

import type { Helpers } from "./helpers";
import { dispatchThen } from "./updates";

declare function __cleat_child_workflow_with_options(name: string, input: string, version: number, priority: number, parentClosePolicy: string): string;
declare function __cleat_await_all_children(runIdsJson: string): string;
declare function __cleat_await_any_child(runIdsJson: string): string;
declare function __cleat_poll_child(runId: string): string;

/** Per-child options the host reads. An option it does not read is refused by `startChildWithOptions`, not ignored. */
export interface ChildOptions {
  /** Pin the child's workflow-definition version. Omitted (or 0) leaves it to the engine's binding policy. */
  version?: number;
  /** Scheduling priority for the child; lower numbers are picked first. Omitted (or 0) is the host's default. */
  priority?: number;
  /**
   * What happens to the child when this run closes. UPPERCASE, exactly as the engine stores it, and not the camelCase the option
   * NAME is: the engine rejects a mis-cased value rather than normalising it (cleat#936), because `'terminate'` MATCHED on MySQL
   * and did not on PostgreSQL, giving the same workflow opposite behaviour per database. Omitted is the host's default, ABANDON.
   */
  parentClosePolicy?: "ABANDON" | "TERMINATE" | "REQUEST_CANCEL";
}

/** One child's settled state, `Promise.allSettled`-shaped. `runId` is always present, so a caller can map an entry back to the id it asked about. */
export type ChildOutcome<T = unknown> =
  | { runId: string; status: "fulfilled"; value: T }
  | { runId: string; status: "rejected"; reason: Error };

/** One child's state right now. `"pending"` is explicit: a still-running child is neither a value nor a failure. */
export type ChildSnapshot<T = unknown> =
  | { runId: string; status: "pending" }
  | { runId: string; status: "fulfilled"; value: T }
  | { runId: string; status: "rejected"; reason: Error };

/** What `awaitAny` resolves with: which child won, and its value. */
export interface ChildWinner<T = unknown> {
  runId: string;
  value: T;
}

/** A child's entry as the host writes it, before `result` is parsed and `error` is turned into a rejection. */
interface HostOutcome {
  run_id: string;
  result?: string;
  error?: string;
}

interface HostPoll {
  status: "running" | "completed" | "failed";
  result?: string;
  error?: string;
}

interface Envelope<T> {
  ok?: T;
  err?: string;
  suspended?: true;
}

const POLICIES = ["ABANDON", "TERMINATE", "REQUEST_CANCEL"] as const;

/**
 * The host's `err` turned into the same `CleatError` that every other call rejects with, without this module being allowed to
 * import `index.ts` (which exports the class and imports this file back). `settle` is the one place that construction happens, so
 * it is asked to do it with the host's own message; the catch below never leaves anything unhandled.
 */
async function rejection(h: Helpers, message: string): Promise<never> {
  return h.settle<never>(JSON.stringify({ err: message }));
}

/** Tag an error with the child it came from, so a caller holding a bare rejection still knows which waiter failed. */
function tag(error: unknown, runId: string): Error {
  const e = error instanceof Error ? error : new Error(String(error));
  (e as Error & { childRunId?: string }).childRunId = runId;
  return e;
}

export interface ChildrenCtx {
  /**
   * Start a child workflow and return its run id, without waiting for it.
   *
   * Unlike `Ctx.child`, this does not await the child: pass the ids to `awaitAll`/`awaitAny`, or read them with `pollChild`.
   * `version` pins the child's definition; omit it to let the engine's binding policy decide. `priority` is the child's
   * scheduling priority, lower first. `parentClosePolicy` says what happens to the child when this run closes, in the engine's own UPPERCASE spelling.
   *
   * An option the host does not read is REFUSED here rather than dropped, and so is a `parentClosePolicy` outside the three the
   * ABI defines: a typo in either would otherwise start a child with a policy nobody chose.
   *
   * Replays from the recorded `child_workflow` event like `Ctx.child`: the id is the one the first execution got.
   */
  startChildWithOptions<T = unknown>(name: string, input?: unknown, options?: ChildOptions): Promise<string>;
  /**
   * Wait until every named child is finished, and answer their outcomes in the order the ids were given.
   *
   * Shaped like `Promise.allSettled`: it resolves even when children failed, each failure in its entry's `reason`, so one
   * broken child does not cost the caller the results of its siblings. It SUSPENDS the workflow until the last child is done,
   * and dispatches pending updates while it waits.
   */
  awaitAll<T = unknown>(runIds: string[]): Promise<ChildOutcome<T>[]>;
  /**
   * Wait until ANY named child is finished, and answer which one and its value.
   *
   * A child that FINISHED BY FAILING also ends the wait, and rejects with a `CleatError` carrying that child's message and a
   * `childRunId` property naming it -- the host does not go on to a sibling, and pretending otherwise here would wait for a child
   * the caller believes is still relevant. When several are already finished the winner is the lowest-sorting run id (the host
   * sorts before polling, so replay picks the same one).
   *
   * An empty list is refused: nothing could ever finish, so the wait would suspend forever.
   *
   * SUSPENDS the workflow until a child finishes, and dispatches pending updates while it waits.
   */
  awaitAny<T = unknown>(runIds: string[]): Promise<ChildWinner<T>>;
  /**
   * Read one child's state now, without waiting: `"pending"`, or a value, or a rejection.
   *
   * Synchronous, because the host answers it without suspending, and replayable: the host compares the child's completion
   * instant against this run's durable clock, so a replay answers what the first execution saw rather than what is true now.
   *
   * A HOST error -- an unknown run id, or a store that could not answer -- is THROWN rather than reported as a failed child,
   * because those are different facts (cleat#3387). It is thrown as a plain `Error` and not a `CleatError`: this call has no
   * promise through which to reject, and the class is constructed in `index.ts`, which a group may not import. The host's own
   * message is preserved either way.
   */
  pollChild<T = unknown>(runId: string): ChildSnapshot<T>;
}

export function makeChildren(h: Helpers): ChildrenCtx {
  return {
    startChildWithOptions: <T>(name: string, input?: unknown, options: ChildOptions = {}): Promise<string> => {
      for (const key of Object.keys(options)) {
        if (!["version", "priority", "parentClosePolicy"].includes(key)) {
          throw new TypeError(`startChildWithOptions: the host does not read the option "${key}"`);
        }
      }
      const policy = options.parentClosePolicy ?? "";
      if (policy !== "" && !(POLICIES as readonly string[]).includes(policy)) {
        throw new TypeError(`startChildWithOptions: parentClosePolicy must be one of ${POLICIES.join(", ")}, got "${policy}"`);
      }
      return h.settle<string>(__cleat_child_workflow_with_options(name, h.encode(input ?? null), options.version ?? 0, options.priority ?? 0, policy));
    },

    awaitAll: <T>(runIds: string[]): Promise<ChildOutcome<T>[]> =>
      dispatchThen(() =>
        h.settle<HostOutcome[]>(__cleat_await_all_children(h.encode(runIds))).then((rows) =>
          // The host answers in the caller's order, so this map preserves it. Each entry is settled through `settle` rather than
          // hand-built, so a failed child gets the same `CleatError` every other call would give for that message.
          Promise.all(
            rows.map((row): Promise<ChildOutcome<T>> =>
              rejectionOrValue(h, row).then(
                (value) => ({ runId: row.run_id, status: "fulfilled", value }),
                (reason) => ({ runId: row.run_id, status: "rejected", reason: tag(reason, row.run_id) }),
              ),
            ),
          ),
        ),
      ),

    awaitAny: <T>(runIds: string[]): Promise<ChildWinner<T>> => {
      if (runIds.length === 0) throw new TypeError("awaitAny: the run id list is empty, so no child could ever finish");
      return dispatchThen(() =>
        h.settle<HostOutcome>(__cleat_await_any_child(h.encode(runIds))).then((row) =>
          rejectionOrValue(h, row).then(
            (value) => ({ runId: row.run_id, value }),
            (reason) => Promise.reject(tag(reason, row.run_id)),
          ),
        ),
      );
    },

    pollChild: <T>(runId: string): ChildSnapshot<T> => {
      const r = JSON.parse(__cleat_poll_child(runId)) as Envelope<HostPoll>;
      if (r.suspended) throw new Error("pollChild: this segment has suspended, so the host has no answer");
      if (r.err !== undefined) throw new Error(r.err);
      const p = r.ok;
      // Neither ok nor err nor suspended is a host that answered nothing; treating it as "pending" would wait forever.
      if (p === undefined) throw new Error("pollChild: the host answered neither a status nor an error");
      switch (p.status) {
        case "running":
          return { runId, status: "pending" };
        case "completed":
          return { runId, status: "fulfilled", value: h.parseJSON(p.result ?? "") as T };
        case "failed":
          return { runId, status: "rejected", reason: new Error(p.error ?? "") };
        default:
          // A status this layer does not know is a host that changed shape under it; saying "pending" would wait forever.
          throw new Error(`pollChild: the host answered with an unknown status "${(p as { status: string }).status}"`);
      }
    },
  };
}

/**
 * One host outcome as a promise: the child's parsed value, or a rejection carrying the host's message.
 *
 * Both branches go through `settle` -- the value from an `ok` envelope, the failure from an `err` one -- so `awaitAll` produces
 * exactly the error type its siblings reject with. A row with neither `result` nor `error` is a completed child whose result was
 * empty, which the wire format cannot distinguish from a child that failed with no message; it is treated as the former, and the
 * ambiguity is the host's to fix rather than something to invent an error for.
 */
function rejectionOrValue(h: Helpers, row: HostOutcome): Promise<unknown> {
  if (row.error !== undefined) return rejection(h, row.error);
  return h.settle<string>(JSON.stringify({ ok: row.result ?? "" })).then((text) => h.parseJSON(text));
}
