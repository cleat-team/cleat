declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/index/__durable_inner_place_order
   * @param h `../../packages/cleat-as/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_place_order(h: __Internref4, input: string): string;
  /**
   * assembly/index/__durable_inner_cancel_order
   * @param h `../../packages/cleat-as/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_cancel_order(h: __Internref4, input: string): string;
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
/** ../../packages/cleat-as/assembly/host-calls/HostCalls */
declare class __Internref4 extends Number {
  private __nominal4: symbol;
  private __nominal0: symbol;
}
/** Instantiates the compiled WebAssembly module with the given imports. */
export declare function instantiate(module: WebAssembly.Module, imports: {
  env: unknown,
}): Promise<typeof __AdaptedExports>;
