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

/** WASM host ABI version this module targets. */
export const ABI_VERSION: i32 = 4;

/** JSON string of plugin dependencies (map of name -> semver constraint).
 *  Example: '{"llm":">=1.2.0","blobstore":"~2.0.0"}'
 */
export const PLUGIN_DEPS: string = "{}";

/** Child workflow version binding policy.
 *  Values: "", "frozen", "stable", "latest", or "tag:<name>"
 */
export const CHILD_BINDING_POLICY: string = "";
