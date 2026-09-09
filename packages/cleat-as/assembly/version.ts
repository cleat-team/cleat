/**
 * Build-time version constants for cleat AssemblyScript workflows.
 *
 * These constants are compiled into the WASM module and can be read by the
 * host from the "cleat.metadata" custom section after the post-compile
 * inject-metadata.js script has been run.
 *
 * The actual values are substituted by the build system before compilation.
 */

/** Human-readable name of this workflow definition. */
export const WORKFLOW_NAME: string = "unknown";

/** Monotonic version number for this workflow definition. */
export const WORKFLOW_VERSION: i32 = 0;

/** Minimum compatible workflow definition version (for child workflows). */
export const MIN_COMPATIBLE_VERSION: i32 = 1;

// ABI_VERSION was declared here as `i32 = 4` and removed in cleat#1078.
//
// It was public SDK surface -- index.ts does `export * from "./version"` -- and
// nothing read it: not this package, not inject-metadata.js, not the host. Every
// write path emits 0 or 1, and the host has only ever known 1
// (wasm/metadata.go's CurrentABIVersion). So the SDK published a specific ABI
// claim that nothing produced and nothing accepted.
//
// Removed rather than corrected to 1, deliberately: setting it to 1 leaves a
// value that LOOKS authoritative, is still read by nothing, and would silently
// go stale the next time the real ABI moves. The metadata section's abi_version
// is written by inject-metadata.js from --abi-version / CLEAT_ABI_VERSION,
// which is a different mechanism entirely and never consulted this constant.

/** JSON string of plugin dependencies (map of name -> semver constraint).
 *  Example: '{"llm":">=1.2.0","blobstore":"~2.0.0"}'
 */
export const PLUGIN_DEPS: string = "{}";

/** Child workflow version binding policy.
 *  Values: "", "frozen", "stable", "latest", or "tag:<name>"
 */
export const CHILD_BINDING_POLICY: string = "";
