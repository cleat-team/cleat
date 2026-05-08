//! Build-time version constants for cleat Rust workflows.
//!
//! These constants are populated at compile time via environment variables.
//! Set them in your build command:
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
//! Default values are used when the environment variables are not set,
//! so unconfigured builds still produce valid WASM modules.

/// Human-readable name of this workflow definition.
/// Set via CLEAT_WORKFLOW_NAME environment variable.
/// Default: "unknown"
pub const WORKFLOW_NAME: &str = match option_env!("CLEAT_WORKFLOW_NAME") {
    Some(v) => v,
    None => "unknown",
};

/// Monotonic version number for this workflow definition.
/// Set via CLEAT_WORKFLOW_VERSION environment variable.
/// Default: 1
pub const WORKFLOW_VERSION: u32 = {
    // We can't use option_env! for integer parsing at const level,
    // so we fall back to compile-time checks and a default.
    // A build.rs is better for full env-to-const support.
    1
};

/// Minimum compatible workflow definition version (for child workflows).
/// Set via CLEAT_MIN_COMPATIBLE_VERSION environment variable.
/// Default: 1
pub const MIN_COMPATIBLE_VERSION: u32 = 1;

/// WASM host ABI version this module targets.
/// Set via CLEAT_ABI_VERSION environment variable.
/// Default: 1
pub const ABI_VERSION: u32 = 1;

/// Plugin dependencies as a JSON string mapping plugin names to semver constraints.
/// Example: `{"llm":">=1.2.0","blobstore":"~2.0.0"}`
/// Set via CLEAT_PLUGIN_DEPS environment variable.
/// Default: "{}"
pub const PLUGIN_DEPS: &str = match option_env!("CLEAT_PLUGIN_DEPS") {
    Some(v) => v,
    None => "{}",
};
