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

    // Empty is not zero. The build.rs fallbacks supply "1", so an empty value
    // means something set the variable to nothing -- a shell expansion that
    // produced no output, most often -- and silently compiling that to 0 makes
    // a workflow claim version 0.
    if bytes.is_empty() {
        panic!(
            "cleat version environment variable is empty; expected a decimal integer such as \"3\""
        );
    }

    let mut val: u32 = 0;
    let mut i = 0;
    while i < bytes.len() {
        let b = bytes[i];

        // Reject rather than skip. This loop used to `if digit { accumulate }`
        // with no `else` and no `break`, so a non-digit was stepped over and
        // accumulation continued across it -- digits on either side of the
        // junk concatenated. Measured on the real crate through build.rs and
        // env!, 2026-09-09:
        //
        //     CLEAT_ABI_VERSION=1.2.3            -> ABI_VERSION            = 123
        //     CLEAT_WORKFLOW_VERSION=-1          -> WORKFLOW_VERSION       = 1
        //     CLEAT_MIN_COMPATIBLE_VERSION=v2    -> MIN_COMPATIBLE_VERSION = 2
        //
        // A semver is the realistic input here, and 1.2.3 -> 123 is the worst
        // shape: plausible, wrong, and silent. Note this is strictly worse than
        // a parser that stops at the first bad byte, which at least truncates
        // predictably.
        //
        // panic! in a const fn is a COMPILE error, because all three callers
        // are const initialisers -- there is no runtime path into here. So a
        // malformed version fails the build, which is when you want to know.
        if b < b'0' || b > b'9' {
            panic!("cleat version environment variable is not a decimal integer; expected only ASCII digits, e.g. \"3\"");
        }

        // Overflow is also a const-eval error rather than a wrap. Left to the
        // compiler deliberately: an explicit check here would report the same
        // condition with less precision than rustc's own message, which names
        // the operation and the value.
        val = val * 10 + (b - b'0') as u32;
        i += 1;
    }
    val
}

#[cfg(test)]
mod tests {
    use super::*;

    // parse_u32 is a `const fn`, which means it is ALSO an ordinary function.
    // In the three call sites above it is const-evaluated, so a panic there is
    // a compile error and cannot be asserted from a test. Calling it at runtime
    // exercises the same code and lets the rejection be pinned.

    #[test]
    fn a_decimal_string_parses() {
        assert_eq!(parse_u32("0"), 0);
        assert_eq!(parse_u32("1"), 1);
        assert_eq!(parse_u32("42"), 42);
        assert_eq!(parse_u32("0007"), 7, "leading zeros are still decimal");
        assert_eq!(parse_u32("4294967295"), u32::MAX);
    }

    // The regression. Each of these used to return a plausible number, measured
    // on the real crate through build.rs and env! on 2026-09-09:
    //   "1.2.3" -> 123, "-1" -> 1, "v2" -> 2, "" -> 0
    // The semver case is the one to keep: it is the input a person is most
    // likely to supply by mistake, and 123 is a number a version comparison
    // will happily accept.

    #[test]
    #[should_panic(expected = "not a decimal integer")]
    fn a_semver_is_rejected_rather_than_concatenated() {
        parse_u32("1.2.3");
    }

    #[test]
    #[should_panic(expected = "not a decimal integer")]
    fn a_negative_is_rejected_rather_than_losing_its_sign() {
        parse_u32("-1");
    }

    #[test]
    #[should_panic(expected = "not a decimal integer")]
    fn junk_between_digits_is_rejected_rather_than_skipped() {
        parse_u32("1x2");
    }

    // Empty is its own case: it is the one that produced a *valid-looking* 0
    // rather than a wrong non-zero, so it would survive any check that only
    // asks "is this number plausible".
    #[test]
    #[should_panic(expected = "is empty")]
    fn empty_is_rejected_rather_than_read_as_zero() {
        parse_u32("");
    }
}
