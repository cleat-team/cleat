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
export function durableEntry(name?: string): (target: any, propertyKey?: string, descriptor?: any) => void {
  return function (
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    _target: any,
    _propertyKey?: string,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    _descriptor?: any,
  ): void {
    // Marker decorator — no runtime behavior.
    // The transformer plugin reads this decorator metadata at compile time
    // to generate the appropriate WASM export with the ABI signature.
  };
}
