declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/workflows/checkoutWorkflow
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function checkoutWorkflow(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
  /**
   * assembly/workflows/dispatchOrder
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function dispatchOrder(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
}
/** Instantiates the compiled WebAssembly module with the given imports. */
export declare function instantiate(module: WebAssembly.Module, imports: {
  env: unknown,
}): Promise<typeof __AdaptedExports>;
