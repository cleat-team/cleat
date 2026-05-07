/**
 * `@cleat/sdk` — AssemblyScript SDK for the cleat durable execution framework.
 *
 * This package provides the ABI-compatible bindings needed to write cleat
 * workflows in AssemblyScript that compile to WebAssembly.
 *
 * ## Quick Start
 *
 * ```ts
 * import { HostCalls, Memory, encodeExportResult, SUSPEND_SENTINEL } from "@cleat/sdk";
 *
 * let host = new HostCalls();
 *
 * export function myWorkflow(argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32): i64 {
 *   host.log("workflow started");
 *   let outcome = host.durableCall("payment", "charge", `{"amount": 100}`);
 *   if (outcome.isError) {
 *     return encodeExportResult(1, 0); // error
 *   }
 *   // Write output to outPtr...
 *   return encodeExportResult(0, bytesWritten);
 * }
 * ```
 *
 * ## Modules
 *
 * - `memory.ts` — Memory layout constants, string I/O helpers, and bit-packing
 *   decoders for all 15 host function result types.
 * - `host-calls.ts` — Raw `@external` import declarations and the `HostCalls`
 *   class with idiomatic AssemblyScript methods for each host function.
 * - `durable-entry.ts` — Marker decorator for workflow entry points (used by
 *   the cleat-as-transformer plugin).
 *
 * @packageDocumentation
 */

export * from "./memory";
export * from "./host-calls";
export * from "./durable-entry";
export * from "./plugins";
