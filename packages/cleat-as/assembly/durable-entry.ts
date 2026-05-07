/**
 * Marker decorator for workflow entry points.
 *
 * Identifies a function as a cleat workflow entry point. The actual WASM
 * export generation with the ABI-compatible signature
 * `(argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32) -> i64` is
 * handled by the cleat-as-transformer plugin at compile time.
 *
 * Usage:
 * ```ts
 * &#64;durableEntry("PlaceOrder")
 * export function placeOrder(argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32): i64 {
 *   // workflow logic
 * }
 * ```
 *
 * @param name - Optional workflow name. Defaults to the function name.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function durableEntry(name: string = ""): (target: usize, propertyKey: string, descriptor: usize) => void {
  return function (
    _target: usize,
    _propertyKey: string,
    _descriptor: usize,
  ): void {
    // Marker decorator — no runtime behavior.
    // The transformer plugin reads this decorator metadata at compile time
    // to generate the appropriate WASM export with the ABI signature.
  };
}

// ═══════════════════════════════════════════════
// TerminalError
// ═══════════════════════════════════════════════

/**
 * A non-retryable error that workflows can return to signal a terminal
 * failure.
 *
 * When a workflow function returns a `TerminalError` instance, the
 * @durableEntry transform-generated wrapper detects it and returns
 * `encodeExportResult(TERMINAL_ERROR_CODE, 0)` to the host, indicating
 * that the workflow should NOT be retried.
 *
 * Compatible with --runtime stub: since AssemblyScript does not support
 * try/catch or exceptions in stub mode, the `TerminalError` is returned
 * as a value rather than thrown. The transform checks the return type.
 *
 * Usage:
 * ```ts
 * import { TerminalError } from "@cleat/sdk";
 *
 * &#64;durableEntry()
 * export function myWorkflow(argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32): i64 {
 *   let host = new HostCalls();
 *   let result = host.durableCall("payment", "charge", `{"amount": 100}`);
 *   if (result.isError) {
 *     // Signal a non-retryable error — workflow will not be retried
 *     return new TerminalError("payment service unavailable, not retrying");
 *   }
 *   // ... continue with success path
 * }
 * ```
 */
export class TerminalError {
  /**
   * @param message - Human-readable error description.
   */
  constructor(public readonly message: string) {}

  /**
   * Returns a string representation of this terminal error.
   */
  toString(): string {
    return "TerminalError: " + this.message;
  }
}
