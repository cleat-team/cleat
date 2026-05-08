declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/index/place_order
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function place_order(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
  /**
   * assembly/index/cancel_order
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function cancel_order(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
}
/** Instantiates the compiled WebAssembly module with the given imports. */
export declare function instantiate(module: WebAssembly.Module, imports: {
  env: unknown,
}): Promise<typeof __AdaptedExports>;
