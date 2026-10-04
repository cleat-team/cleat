declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/index/__durable_inner_exercise_host_call
   * @param h `~lib/@cleat/sdk/assembly/host-calls/HostCalls`
   * @param input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_exercise_host_call(h: __Internref5, input: string): string;
  /**
   * assembly/index/exercise_host_call
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function exercise_host_call(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
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
