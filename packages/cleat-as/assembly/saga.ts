/**
 * Saga pattern for structured compensation in AssemblyScript.
 *
 * Ported from the Go Saga at durable/runtime.go.
 *
 * ## Usage (with named functions — required by AS constraint #13)
 *
 * ```ts
 * import { HostCalls, Saga } from "@cleat/sdk";
 *
 * function reserveAction(h: HostCalls): string {
 *   let r = h.durableCall("inventory", "Reserve", '{"items":["A","B"]}');
 *   if (r.isError) return r.error;
 *   return ""; // success
 * }
 *
 * function reserveCompensate(h: HostCalls): void {
 *   h.durableCall("inventory", "Release", '{"reservation_id":"..."}');
 * }
 *
 * function myWorkflow(h: HostCalls, input: string): string {
 *   let saga = new Saga();
 *   saga.addStep("reserve", reserveAction, reserveCompensate);
 *   saga.addStep("charge", chargeAction, chargeCompensate);
 *   let result = saga.run(h);
 *   if (result.length > 0) return result; // error
 *   return '{"status":"ok"}';
 * }
 * ```
 *
 * ## Compensation behavior
 *
 * Steps execute in order. If any step fails (returns non-empty error string),
 * all previously completed steps are compensated in reverse order.
 *
 * ## Constraints
 *
 * - `--runtime stub` compatible (no exceptions or try/catch used).
 * - Uses function references, not closures. All action and compensation
 *   functions must be named top-level functions.
 * - Generics are NOT supported for saga result collection. AssemblyScript's
 *   generics system (especially when targeting WASM with `--runtime stub`)
 *   does not support the type-level patterns needed for `Saga<T>` with typed
 *   result collection. Use the Go `SagaTyped[T]` or Java `Saga.SagaTyped<T>`
 *   when typed results are needed.
 *
 * @packageDocumentation
 */

import { HostCalls } from "./host-calls";
import { isWorkflowSuspended } from "./memory";

/**
 * A single saga step with its forward action and compensation function.
 *
 * `forward` returns "" on success or an error message on failure.
 * `compensate` is a cleanup action called when a later step fails.
 * Pass null for compensate if the step has no meaningful compensation
 * (e.g., sending a notification is best-effort).
 */
export class SagaStep {
  constructor(
    /** Human-readable description for logging. */
    public readonly description: string,
    /**
     * Forward action. Takes a HostCalls, returns "" on success
     * or an error message on failure.
     */
    public readonly forward: (h: HostCalls) => string,
    /**
     * Compensation action. Takes a HostCalls. Called when a later
     * step fails. Pass null for steps with no meaningful compensation.
     */
    public readonly compensate: (h: HostCalls) => void | null,
  ) {}
}

/**
 * Provides structured compensation for multi-step operations.
 *
 * Steps execute in order. If any step fails, all previously completed
 * steps are compensated in reverse order.
 *
 * Usage:
 * ```ts
 * let s = new Saga();
 * s.addStep("charge", chargeFn, refundFn);
 * s.addStep("ship", shipFn, cancelShipFn);
 * let err = s.run(h);
 * if (err.length > 0) { /* handle error *\/ }
 * ```
 */
export class Saga {
  private steps: SagaStep[] = [];

  /**
   * Add a step to the saga.
   *
   * @param description - Human-readable description for logging.
   * @param forward     - Main action. Returns "" on success, error message on failure.
   * @param compensate  - Cleanup action. Called on a later step's failure.
   *                      Pass null for best-effort steps with no compensation.
   * @returns The Saga instance for chaining.
   */
  addStep(
    description: string,
    forward: (h: HostCalls) => string,
    compensate: (h: HostCalls) => void | null,
  ): Saga {
    this.steps.push(new SagaStep(description, forward, compensate));
    return this;
  }

  /**
   * Execute all forward steps in order.
   *
   * If any step fails (returns non-empty string), previously completed
   * steps are compensated in reverse order. Null compensate functions
   * are skipped.
   *
   * @param h - HostCalls instance for making workflow calls.
   * @returns "" on success, or an error message on failure.
   *
   * On suspension (durableSleep or awaitChild suspend during execution),
   * the step will set the workflowSuspended flag. The caller should check
   * isWorkflowSuspended() after saga.run() returns and return the SUSPEND_SENTINEL
   * if the workflow suspended mid-execution.
   */
  run(h: HostCalls): string {
    let completed: i32 = 0;

    for (let i: i32 = 0; i < this.steps.length; i++) {
      let step: SagaStep = this.steps[i];
      let result: string = step.forward(h);

      // Check if the workflow suspended during this step
      if (isWorkflowSuspended()) {
        return "";
      }

      // Non-empty result means error
      if (result.length > 0) {
        // Step failed — compensate in reverse order
        this.compensateReverse(h, completed);
        return result;
      }

      completed++;
    }

    return ""; // all steps succeeded
  }

  /**
   * Compensate completed steps in reverse order.
   * Skips null compensate functions.
   */
  private compensateReverse(h: HostCalls, completed: i32): void {
    for (let j: i32 = completed - 1; j >= 0; j--) {
      let cs: SagaStep = this.steps[j];
      if (cs.compensate !== null) {
        cs.compensate(h);
      }
    }
  }
}
