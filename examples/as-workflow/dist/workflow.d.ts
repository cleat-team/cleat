declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/index/__durable_inner_place_order
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_place_order(h: __Internref5, input: string): string;
  /**
   * assembly/index/__durable_inner_cancel_order
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_cancel_order(h: __Internref5, input: string): string;
  /**
   * assembly/index/__durable_inner_defer_order
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_defer_order(h: __Internref5, input: string): string;
  /**
   * assembly/index/__durable_inner_defer_suspend
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_defer_suspend(h: __Internref5, input: string): string;
  /**
   * assembly/index/__durable_inner_spin_forever
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_spin_forever(h: __Internref5, input: string): string;
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
  /**
   * assembly/index/defer_order
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function defer_order(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
  /**
   * assembly/index/defer_suspend
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function defer_suspend(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
  /**
   * assembly/index/spin_forever
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function spin_forever(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
  /**
   * assembly/index/__cleat_run_deferred
   * @returns `i64`
   */
  export function __cleat_run_deferred(): bigint;
}
/** ~lib/@cleat/sdk/assembly/host-calls/HostCalls */
declare class __Internref5 extends Number {
  private __nominal5: symbol;
  private __nominal0: symbol;
}
/** Instantiates the compiled WebAssembly module with the given imports. */
export declare function instantiate(module: WebAssembly.Module, imports: {
  env: unknown,
}): Promise<typeof __AdaptedExports>;
