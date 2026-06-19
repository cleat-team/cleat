declare namespace __AdaptedExports {
  /** Exported memory */
  export const memory: WebAssembly.Memory;
  /**
   * assembly/index/__durable_inner_call_all_plugins
   * @param h `../../../../packages/cleat-as/assembly/host-calls/HostCalls`
   * @param _input `~lib/string/String`
   * @returns `~lib/string/String`
   */
  export function __durable_inner_call_all_plugins(h: __Internref4, _input: string): string;
  /**
   * assembly/index/call_all_plugins
   * @param argsPtr `usize`
   * @param argsLen `i32`
   * @param outPtr `usize`
   * @param maxOutLen `i32`
   * @returns `i64`
   */
  export function call_all_plugins(argsPtr: number, argsLen: number, outPtr: number, maxOutLen: number): bigint;
}
/** ../../../../packages/cleat-as/assembly/host-calls/HostCalls */
declare class __Internref4 extends Number {
  private __nominal4: symbol;
  private __nominal0: symbol;
}
/** Instantiates the compiled WebAssembly module with the given imports. */
export declare function instantiate(module: WebAssembly.Module, imports: {
  env: unknown,
}): Promise<typeof __AdaptedExports>;
