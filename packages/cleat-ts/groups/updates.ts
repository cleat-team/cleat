// The `updates` group of host-call bindings (cleat#3827): workflow updates, a request/reply call into a RUNNING workflow that
// can both change its state and return a value to the caller.
//
// THE DISPATCH IS HERE, NOT IN THE PLUGIN. A handler is an ordinary async function and may await a durable call, so Rust cannot
// run it: it would have to wait for a promise only this event loop settles. The plugin binds `cleat_register_update_handler`,
// `cleat_poll_update` and `cleat_complete_update`, and this file does what `cleat-sdk`'s `dispatch_updates` does for Rust.
//
// WHERE IT RUNS. A host-delivered update cannot interrupt a workflow; something in the guest has to ask. It has to ask at a fixed
// PROGRAM POSITION, not at a moment in time, because replay re-executes the guest and matches each host call against the recorded
// history in order (engine/updater.go). So, exactly as the Rust and Go SDKs do, `dispatchThen` asks immediately BEFORE each call
// that can suspend the workflow -- `sleep`, `waitForSignal`, the wait inside `child`, `awaitPromise` -- and `dispatchUpdates()` is
// there for a workflow that wants more. An update is therefore handled at the next such point and not the instant it arrives.
//
// THE POLL IS SYNCHRONOUS, and that is what keeps a workflow that never receives an update unchanged: `__cleat_poll_update()`
// answers `null` when nothing is pending, and then the call proceeds in the same tick with no extra `await`. Only a delivery
// makes anything asynchronous.
//
// EVERY DELIVERED UPDATE IS ANSWERED, as in Rust: a name with no handler, a validator that refuses and a handler that throws all
// complete the request with an error. Leaving one uncompleted would leave the caller holding a promise nothing settles.
//
// THE HANDLER RE-RUNS ON EVERY REPLAY, and that is the point: the durable facts are its input and its output, not its execution,
// and re-running it is what rebuilds the state it changed. Durable calls the handler makes replay in order like any others.

import type { Helpers } from "./helpers";

declare function __cleat_register_update_handler(name: string): string;
declare function __cleat_poll_update(): string;
declare function __cleat_complete_update(requestId: string, resultJson: string, errMsg: string): string;

export interface UpdatesCtx {
  /**
   * Register the handler for updates named `name`. It receives the update's payload (parsed as JSON when it is JSON, otherwise
   * the string as sent) and returns the value the caller gets back; a throw rejects the caller with the error's message.
   *
   * `validate`, when given, runs FIRST and decides whether the update is accepted: it must have no effect on the workflow, and a
   * throw refuses the update with that message without running the handler. That read-only first step is what makes an update
   * different from a signal.
   *
   * Registration is a recorded step, so register at the same point on every run (normally at the top of the workflow) and not
   * conditionally on something that can differ across a replay. A handler that returns nothing answers the caller with `null`.
   */
  registerUpdateHandler<I = unknown, O = unknown>(name: string, handler: (input: I) => O | Promise<O>, validate?: (input: I) => void | Promise<void>): Promise<void>;
  /**
   * Deliver and run every update pending for this run now. The SDK already does this before each call that can suspend, so an
   * ordinary workflow needs no update-specific code; call it where you want an update handled at a point of your own choosing.
   */
  dispatchUpdates(): Promise<void>;
}

/** What `__cleat_poll_update` hands back for one delivery. `request_id` is opaque: it goes back to `complete` unchanged. */
interface Delivery {
  name: string;
  payload: string;
  request_id: string;
}

type Handler = { run: (input: never) => unknown; validate?: (input: never) => void | Promise<void> };

const handlers = new Map<string, Handler>();
let helpers: Helpers;
// Re-entrancy guard. Every dispatch point is a suspension point and a handler is ordinary workflow code that may sleep or await,
// so without this a handler doing either would re-enter dispatch and recurse. Nesting would also be wrong if it terminated: the
// inner dispatch would interleave a second update's events inside the first one's, and the received/completed pair would no
// longer bracket the handler that produced it.
let dispatching = false;

type Polled = { none: true } | { suspended: true } | { delivery: Delivery };

function poll(): Polled {
  const r = JSON.parse(__cleat_poll_update()) as { ok?: Delivery | null; err?: string; suspended?: true };
  if (r.suspended) return { suspended: true };
  // An error from the host (a defer phase refuses new work; an unreadable envelope) is "nothing to deliver": the call this was
  // placed before goes ahead, as it does in Rust, where `poll_update` reporting an error ends the dispatch loop.
  if (r.err !== undefined || r.ok === null || r.ok === undefined) return { none: true };
  return { delivery: r.ok };
}

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

async function runOne(d: Delivery): Promise<void> {
  const entry = handlers.get(d.name);
  let result = "null";
  let failure = "";
  if (entry === undefined) {
    failure = `cleat: no update handler registered for "${d.name}"`;
  } else {
    try {
      const input = helpers.parseJSON(d.payload) as never;
      if (entry.validate) await entry.validate(input);
      const out = await entry.run(input);
      result = helpers.encode(out === undefined ? null : out);
    } catch (e) {
      failure = message(e) || "the update handler failed";
    }
  }
  // Every path completes the request. A completion the host refuses is not retried here: it is the host's answer.
  await helpers.settle<null>(__cleat_complete_update(d.request_id, failure === "" ? result : "", failure)).catch(() => undefined);
}

async function drain(first: Delivery): Promise<void> {
  dispatching = true;
  try {
    let next: Polled = { delivery: first };
    while ("delivery" in next) {
      await runOne(next.delivery);
      next = poll();
    }
    if ("suspended" in next) await new Promise<void>(() => {}); // never settles, as every call made after a suspension
  } finally {
    dispatching = false;
  }
}

/**
 * Run `call` after delivering any pending update. Used by index.ts before each call that can suspend. With nothing pending it
 * is `call()` in the same tick, so a workflow that never receives an update is not changed by it.
 */
export function dispatchThen<T>(call: () => Promise<T>): Promise<T> {
  if (dispatching) return call();
  const p = poll();
  if ("none" in p) return call();
  if ("suspended" in p) return new Promise<T>(() => {});
  return drain(p.delivery).then(call);
}

export function makeUpdates(h: Helpers): UpdatesCtx {
  helpers = h;
  return {
    registerUpdateHandler: <I, O>(name: string, handler: (input: I) => O | Promise<O>, validate?: (input: I) => void | Promise<void>) => {
      handlers.set(name, { run: handler as Handler["run"], validate: validate as Handler["validate"] });
      return h.settle<null>(__cleat_register_update_handler(name)).then(() => undefined);
    },
    dispatchUpdates: () => {
      if (dispatching) return Promise.resolve();
      const p = poll();
      if ("none" in p) return Promise.resolve();
      if ("suspended" in p) return new Promise<void>(() => {});
      return drain(p.delivery);
    },
  };
}
