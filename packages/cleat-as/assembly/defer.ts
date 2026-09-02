/**
 * Guest-side defer registry for AssemblyScript.
 *
 * IMPROVEMENT-PLAN §3.70 for Go, §3.73 for the other SDKs.
 *
 * `HostCalls.defer(description)` sends a *description* across the boundary and
 * nothing else. The host records that a defer exists; no code anywhere can run
 * it, because there is no body to run. That was true of every AssemblyScript
 * workflow while the SDK's own doc comment said it registers "cleanup to run on
 * workflow exit".
 *
 * `deferFunc(h, description, fn, payload)` is the one with a body. The guest
 * runs it itself, in the instance that holds it, at the moment the entry point
 * finishes.
 *
 * ## Why state is passed, not captured
 *
 * The other three SDKs take a closure. This one cannot: the AS SDK builds with
 * `--runtime stub` and deliberately avoids closures throughout — `saga.ts`
 * says so in as many words ("Uses function references, not closures. All action
 * and compensation functions must be named top-level functions"). A capturing
 * `deferFunc(fn)` is not expressible here.
 *
 * So a defer is a *pair*: a top-level function reference and an explicit
 * payload string it will be handed back. Anything the body needs to know has to
 * be in that string.
 *
 * ```ts
 * function releaseLock(h: HostCalls, payload: string): void {
 *   h.cleatCall("locks", "Release", payload);
 * }
 *
 * function myWorkflow(h: HostCalls, input: string): string {
 *   deferFunc(h, "release order lock", releaseLock, '{"lock":"orders-42"}');
 *   // ...
 * }
 * ```
 *
 * This is the same shape `Saga.addStep` already uses, so it is not a new idiom
 * for this SDK — it is the existing one.
 *
 * ## Why this is a free function and not a HostCalls method
 *
 * `deferFunc` needs the registry, and the registry's function type mentions
 * `HostCalls`. Putting the method on `HostCalls` would make `host-calls.ts` and
 * this module import each other, and a cycle between two modules that both have
 * top-level initializers is a start-function ordering hazard under
 * `--runtime stub`. Taking the `HostCalls` as the first argument keeps the
 * dependency pointing one way.
 *
 * ## Why not the suspension path
 *
 * The generated wrapper drains the table when the entry point returns, but NOT
 * when the workflow suspended: a suspended workflow has not exited, its defers
 * are still pending, and firing them at the first sleep would release locks a
 * workflow that is about to continue still holds.
 *
 * Note the shape of that check differs from the other SDKs. Go, Rust, Python
 * and Java all suspend by unwinding — a panic, a raise, a thrown
 * `SuspendSignal` — so their drain sits on a path the unwind skips. This SDK
 * has no exceptions, so suspension is a *flag* (`isWorkflowSuspended()`) and
 * the entry point returns normally either way. The drain has to be guarded
 * explicitly. An unguarded drain here would look correct and would run every
 * defer at the first `cleatSleep`.
 *
 * @packageDocumentation
 */

import { HostCalls } from "./host-calls";
import { DurableResult } from "./host-calls";
import { isWorkflowSuspended } from "./memory";

/**
 * Signature for a defer body.
 *
 * Takes the workflow's `HostCalls` — the same instance, so a body that touches
 * virtual-object state sees the scope prefix the workflow set — and the payload
 * string registered alongside it.
 */
export type DeferFn = (h: HostCalls, payload: string) => void;

/** A registered defer: the host's ID, the body, and the body's payload. */
class DeferEntry {
  constructor(
    /** The ID the host minted for this defer. */
    public readonly id: string,
    /** The body to run. */
    public readonly fn: DeferFn,
    /** The state the body will be handed back. */
    public readonly payload: string,
  ) {}
}

/**
 * Registered defer bodies, in registration order. A module-level array is the
 * right scope: a WASM guest is one instance running one workflow segment.
 */
let _defers: DeferEntry[] = [];

/**
 * Record a defer body under the ID the host minted for it.
 *
 * The ID is not decorative — it is the same one the host recorded in the
 * workflow's deferrals map, so a body keyed by anything else would run but
 * could never be correlated with what the host thinks it registered.
 *
 * Prefer {@link deferFunc}, which mints the ID and registers in one step. This
 * is exported for tests and for callers that have already registered with the
 * host themselves.
 */
export function registerDefer(id: string, fn: DeferFn, payload: string): void {
  _defers.push(new DeferEntry(id, fn, payload));
}

/**
 * Register cleanup *with a body*, to run when the workflow finishes.
 *
 * Calls the host to mint a defer ID — so the host's record of what is pending
 * is the same as it was before this existed — and keeps the body guest-side.
 * If the host call fails, nothing is registered: a body the host does not know
 * about would run cleanup for a defer that, as far as the durable record is
 * concerned, was never taken.
 *
 * @param h           - the workflow's HostCalls.
 * @param description - human-readable description, recorded by the host.
 * @param fn          - a top-level function; see the module docs on why this
 *                      cannot be a closure.
 * @param payload     - everything `fn` will need, as a string.
 * @returns the defer ID on success, or the host's error.
 */
export function deferFunc(
  h: HostCalls,
  description: string,
  fn: DeferFn,
  payload: string,
): DurableResult<string> {
  let registered = h.defer(description);
  if (!registered.isError) {
    registerDefer(registered.value, fn, payload);
  }
  return registered;
}

/**
 * Run registered defer bodies in LIFO order; return how many ran.
 *
 * The table is drained BEFORE the first body runs, which makes this idempotent:
 * a second call runs nothing. That matters because a caller cannot always tell
 * whether the defers already ran, and cleanup that runs twice releases a lock
 * twice or refunds a charge twice.
 *
 * LIFO is not cosmetic. A defer releases what the defer before it acquired, so
 * running them in registration order unwinds the workflow inside-out.
 *
 * Draining stops if a body suspends the workflow. There is nothing else it
 * could do: the remaining bodies belong to a segment that has not finished, and
 * running them now would fire cleanup for work still in flight. The ones
 * already taken off the table are gone, which is the honest consequence of a
 * defer body calling a suspending host function — don't.
 *
 * A body that fails cannot stop the others, because a body cannot fail
 * observably: this SDK has no exceptions. That is a real difference from the
 * other three, where the drain has to catch.
 */
export function runDeferred(h: HostCalls): i32 {
  let taken = _defers;
  _defers = [];

  let ran: i32 = 0;
  for (let i: i32 = taken.length - 1; i >= 0; i--) {
    let entry = taken[i];
    ran++;
    entry.fn(h, entry.payload);
    if (isWorkflowSuspended()) {
      return ran;
    }
  }
  return ran;
}

/**
 * How many defer bodies are registered and not yet run.
 *
 * For tests and for a workflow that wants to assert its own cleanup is where it
 * thinks it is.
 */
export function pendingDeferCount(): i32 {
  return _defers.length;
}

/**
 * Discard registered defer bodies without running them.
 *
 * For tests. A workflow calling this is throwing its cleanup away.
 */
export function resetDefers(): void {
  _defers = [];
}
