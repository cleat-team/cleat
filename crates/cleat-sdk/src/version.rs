//! Build-time version constants for cleat Rust workflows.
//!
//! These constants are populated at compile time via `build.rs`, which reads
//! the following environment variables and sets corresponding `cargo:rustc-env`
//! directives:
//!
//! ```sh
//! CLEAT_WORKFLOW_NAME=PlaceOrder \
//!   CLEAT_WORKFLOW_VERSION=3 \
//!   CLEAT_MIN_COMPATIBLE_VERSION=1 \
//!   CLEAT_ABI_VERSION=1 \
//!   CLEAT_PLUGIN_DEPS='{"llm":">=1.2.0"}' \
//!   cargo build --target wasm32-wasip1
//! ```
//!
//! `build.rs` supplies defaults when variables are omitted, so unconfigured
//! builds still produce valid WASM modules.

/// Human-readable name of this workflow definition.
/// Set via CLEAT_WORKFLOW_NAME environment variable (build.rs fallback: "unknown").
pub const WORKFLOW_NAME: &str = env!("CLEAT_WORKFLOW_NAME");

/// Monotonic version number for this workflow definition.
/// Parsed at compile time from CLEAT_WORKFLOW_VERSION (build.rs fallback: "1").
pub const WORKFLOW_VERSION: u32 = parse_u32(env!("CLEAT_WORKFLOW_VERSION"));

/// Minimum compatible workflow definition version (for child workflows).
/// Parsed at compile time from CLEAT_MIN_COMPATIBLE_VERSION (build.rs fallback: "1").
pub const MIN_COMPATIBLE_VERSION: u32 = parse_u32(env!("CLEAT_MIN_COMPATIBLE_VERSION"));

/// WASM host ABI version this module targets.
/// Parsed at compile time from CLEAT_ABI_VERSION (build.rs fallback: "1").
pub const ABI_VERSION: u32 = parse_u32(env!("CLEAT_ABI_VERSION"));

/// Plugin dependencies as a JSON string mapping plugin names to semver constraints.
/// Example: `{"llm":">=1.2.0","blobstore":"~2.0.0"}`
/// Set via CLEAT_PLUGIN_DEPS (build.rs fallback: "{}").
pub const PLUGIN_DEPS: &str = env!("CLEAT_PLUGIN_DEPS");

/// Child workflow version binding policy.
/// Values: "", "frozen", "stable", "latest", or "tag:<name>"
/// Set via CLEAT_CHILD_BINDING_POLICY (build.rs fallback: "").
pub const CHILD_BINDING_POLICY: &str = env!("CLEAT_CHILD_BINDING_POLICY");

/// Const-compatible decimal string to u32 parser.
/// Used at compile time to turn `env!("CLEAT_WORKFLOW_VERSION")` etc. into `u32`.
const fn parse_u32(s: &str) -> u32 {
    let bytes = s.as_bytes();
    let mut val: u32 = 0;
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] >= b'0' && bytes[i] <= b'9' {
            val = val * 10 + (bytes[i] - b'0') as u32;
        }
        i += 1;
    }
    val
}
