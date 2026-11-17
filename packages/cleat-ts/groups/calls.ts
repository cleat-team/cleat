// The `calls` group of host-call bindings (cleat#3827): the durable-call variants beyond `Ctx.call`.
//
// FOUR CALLS, TWO SHAPES. `callRetry` and `callHeartbeat` answer a response, or reject with the service's error.
// `send` and `scheduleInvoke` are fire-and-forget: they answer once the host has taken the request and never wait for the work
// to run. None of the four is a dispatch point, which is measured rather than assumed: the Rust and Go SDKs dispatch pending
// updates at `sleep`, the child awaits, the signal awaits and the promise awaits, and at no call -- hence `index.ts` does not
// wrap `call` either (`crates/cleat-sdk/src/host_calls.rs`, `cleat/runtime_workflow.go`).
//
// REPLAY. All four record an event and match it on replay, so a replayed workflow does not re-send or re-schedule anything:
// `call*` matches a call event, `send` a `durable_send` event and `scheduleInvoke` a `durable_schedule_invoke` event. That is
// what makes "fire and forget" mean once and not once per segment.

import type { Helpers } from "./helpers";

declare function __cleat_call_retry(service: string, operation: string, request: string, policyJson: string): string;
declare function __cleat_call_heartbeat(service: string, operation: string, request: string, heartbeatIntervalMs: number): string;
declare function __cleat_send(service: string, operation: string, request: string): string;
declare function __cleat_schedule_invoke(service: string, operation: string, request: string, delayMs: number): string;

/**
 * A retry policy for `callRetry`. Every field is optional and takes the default Go's `DefaultRetryPolicy` gives, so a workflow
 * that names none of them behaves as one written in Go would.
 *
 * The host decides whether the WHOLE policy runs inside one segment, holding the worker slot, and it runs it only while the
 * worst-case backoff fits the tenant's host-retry budget. A longer policy is REFUSED before any call is made -- and in TypeScript
 * that refusal is reported, not retried here: Go and Rust fall back to a loop of their own, but that fallback is broken on
 * replay (cleat#4032), so this binding uses the host-only entry point and rejects with a message naming the policy. A workflow
 * that needs a long backoff should use its own `sleep` between ordinary `call`s.
 */
export interface RetryPolicy {
  /** Total attempts INCLUDING the first call. Default 3. */
  maxAttempts?: number;
  /** Backoff before the second attempt, in milliseconds. Default 1000. */
  initialIntervalMs?: number;
  /** Multiplier applied per further attempt (2 doubles each time). Default 2. */
  backoffCoefficient?: number;
  /** Ceiling on any single backoff, in milliseconds. Default 30000 -- Go's default; the AssemblyScript docs say 60000. */
  maxIntervalMs?: number;
  /**
   * Service error CODES that stop the retry immediately, e.g. `["INVALID_ARGUMENT"]`.
   *
   * CODES, not messages, when the host runs the policy (`engine/helpers.go`, `isDefinitelyNonRetryable` compares
   * `ServiceError.Code` exactly). The SDK-level fallback that runs a too-long policy matches these as SUBSTRINGS of the error
   * message instead -- Rust's doc comment describes the fallback and Go's the host -- so a policy that must behave identically
   * on both paths should use a value that is both a whole code and a unique substring, or a `remember`-style non-retryable
   * error raised by the service itself.
   */
  nonRetryableErrors?: string[];
}

const DEFAULTS: Required<RetryPolicy> = {
  maxAttempts: 3,
  initialIntervalMs: 1000,
  backoffCoefficient: 2,
  maxIntervalMs: 30000,
  nonRetryableErrors: [],
};

export interface CallsCtx {
  /**
   * A durable call that the HOST retries for you, under `policy`, inside ONE segment and ONE history event. It does not
   * suspend while the policy fits the tenant's host-retry budget.
   *
   * Exhausting the policy rejects with a `CleatError` whose message begins `retries exhausted: ` and then the service's last
   * error -- the prefix the worker dead-letters on. An error the policy marks non-retryable is rejected WITHOUT that prefix and
   * without further attempts, because the engine decided not to retry it, and reporting it as exhausted would say it had tried.
   *
   * A policy whose worst-case backoff exceeds the host-retry budget is REFUSED, nothing is called, and the rejection says so:
   * TypeScript does not fall back to an SDK-level loop (cleat#4032). Use a shorter policy, or a loop of your own with `sleep`
   * between attempts.
   *
   * Prefer this to a retry loop of your own when the policy is short: a loop in the workflow records an event per attempt and is
   * replayed attempt by attempt, while the host's loop records one.
   */
  callRetry<T = unknown>(service: string, operation: string, request?: unknown, policy?: RetryPolicy): Promise<T>;
  /**
   * A durable call that heartbeats while it is in flight, so a long-running service operation keeps the worker's lease alive.
   * `heartbeatIntervalMs` has NO default: the host does not invent one, and a liveness interval nobody chose is a liveness
   * policy nobody chose.
   *
   * A heartbeat does not make the call durable against a service that never answers -- the host's own timeouts still apply --
   * and it does not change replay: the call is one recorded event either way.
   */
  callHeartbeat<T = unknown>(service: string, operation: string, request: unknown, heartbeatIntervalMs: number): Promise<T>;
  /**
   * Hand a request to a service and continue WITHOUT waiting for it. Resolves once the host has recorded the send; there is no
   * result and no failure from the work itself, which runs detached from this workflow.
   *
   * RECORDED as a `durable_send` event and matched on replay, so a replayed segment does not send a second time. That is the
   * difference between this and a plain side effect, and the reason to use it for anything a duplicate would matter to.
   */
  send(service: string, operation: string, request?: unknown): Promise<void>;
  /**
   * Schedule the call `service.operation` to run after `delayMs`, and continue immediately. Like `send`, it resolves when the
   * host has recorded it -- recorded as a `durable_schedule_invoke` event, matched on replay -- and the scheduled work is not
   * this workflow's to observe.
   */
  scheduleInvoke(service: string, operation: string, delayMs: number, request?: unknown): Promise<void>;
}

export function makeCalls(h: Helpers): CallsCtx {
  return {
    callRetry: <T>(service: string, operation: string, request?: unknown, policy: RetryPolicy = {}): Promise<T> => {
      const resolved = { ...DEFAULTS, ...policy };
      if (resolved.maxAttempts < 1) throw new TypeError(`callRetry: maxAttempts must be at least 1, got ${resolved.maxAttempts}`);
      // The ABI carries the coefficient in hundredths, and the plugin scales it; a value that is not a number would cross as
      // NaN and be refused by the host with a message about JSON rather than about the policy.
      if (!Number.isFinite(resolved.backoffCoefficient) || resolved.backoffCoefficient < 1) {
        throw new TypeError(`callRetry: backoffCoefficient must be a finite number >= 1, got ${resolved.backoffCoefficient}`);
      }
      return h
        .settle<string>(__cleat_call_retry(service, operation, h.encode(request ?? null), h.encode(resolved)))
        .then((text) => h.parseJSON(text) as T);
    },

    callHeartbeat: <T>(service: string, operation: string, request: unknown, heartbeatIntervalMs: number): Promise<T> => {
      if (!Number.isFinite(heartbeatIntervalMs) || heartbeatIntervalMs <= 0) {
        throw new TypeError(`callHeartbeat: heartbeatIntervalMs must be a positive number, got ${heartbeatIntervalMs}`);
      }
      return h
        .settle<string>(__cleat_call_heartbeat(service, operation, h.encode(request ?? null), heartbeatIntervalMs))
        .then((text) => h.parseJSON(text) as T);
    },

    send: (service: string, operation: string, request?: unknown): Promise<void> =>
      h.settle<null>(__cleat_send(service, operation, h.encode(request ?? null))).then(() => undefined),

    scheduleInvoke: (service: string, operation: string, delayMs: number, request?: unknown): Promise<void> => {
      if (!Number.isFinite(delayMs) || delayMs < 0) {
        throw new TypeError(`scheduleInvoke: delayMs must be a non-negative number, got ${delayMs}`);
      }
      return h.settle<null>(__cleat_schedule_invoke(service, operation, h.encode(request ?? null), delayMs)).then(() => undefined);
    },
  };
}
