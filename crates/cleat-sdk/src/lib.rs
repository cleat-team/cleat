//! Rust SDK for the cleat cleat execution framework.
//!
//! Provides the [`HostCalls`] struct for making cleat API calls from WASM
//! workflows, and memory helpers for the cleat ABI.

pub mod defer;
pub mod host_calls;
pub mod memory;
pub mod plugins;
pub mod saga;
pub mod test;
pub mod version;

// Native stubs for WASM host imports — provided so that crates depending on
// cleat-sdk can compile and link their tests on non-WASM targets. The generated
// #[cleat_entry] wrapper calls HostCalls::json_stringify (and json_parse) which
// in turn call these extern "C" imports. On WASM they're supplied by the host;
// here we provide no-op fallbacks.
#[cfg(not(target_family = "wasm"))]
mod native_stubs {
    #[no_mangle]
    pub extern "C" fn cleat_json_stringify(
        _ptr: *const u8, _len: u32,
        _out_ptr: *mut u8, _out_max_len: u32,
    ) -> i64 {
        0 // return zero bytes written / error
    }

    #[no_mangle]
    pub extern "C" fn cleat_json_parse(
        _json_ptr: *const u8, _json_len: u32,
        _out_ptr: *mut u8, _out_max_len: u32,
    ) -> i64 {
        0 // return zero bytes written / error
    }
}

pub use defer::{register_defer, run_deferred};
pub use cleat_macro::cleat_test;
pub use host_calls::{ChildWorkflowOptions, FetchResult, HostCalls, RetryPolicy, SignalResult};
pub use saga::{Saga, SagaStep};
pub use plugins::{
    AwaitEventResult, AwaitWebhookResult, BlobGetResult, BlobPutResult,
    EvaluateFlagResult, LlmChatMessage, LlmChatResult, LlmTool, LlmToolCall,
    LlmUsageInfo, Plugins, ProduceResult, ResolveIncidentResult,
    SendMessageResult, SendWebhookResult, TriggerIncidentResult,
};

/// Why a host call did not produce a value.
///
/// # Suspension is not a failure
///
/// `Suspended` means the workflow must stop and resume in a later segment --
/// a sleep whose deadline has not arrived, a child that has not finished, a
/// signal that has not come. `#[cleat_entry]` turns it into the host's
/// `SUSPEND_SENTINEL`, never into a workflow error.
///
/// # Why this is a return value and not a panic
///
/// It used to be a panic. `crates/cleat-sdk` raised `SuspendSentinel` through
/// `std::panic::panic_any` and `#[cleat_entry]` caught it with
/// `std::panic::catch_unwind`.
///
/// **`wasm32-wasip1` builds with `panic=abort`.** Ask the compiler rather than
/// believing this comment:
///
/// ```text
/// rustc --print cfg --target wasm32-wasip1 | grep panic     # panic="abort"
/// ```
///
/// There is no unwinding on that target, so `catch_unwind` could never catch
/// anything: the panic aborted, which in WASM is `unreachable`, which is a
/// trap. Every Rust suspension was a trapped guest. It looked healthy because
/// the host sets `session.suspendErr` on the paths that suspend and
/// `engine/executor.go` lets a suspension win over the error beside it, so the
/// trap was discarded and the run reported as a clean suspension. See
/// IMPROVEMENT-PLAN 3.87 for the measurements.
///
/// It also tested green, which is the other half of why it survived: the SDK's
/// own unit tests run on the HOST target, where unwinding works.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CallError {
    /// The workflow must suspend and resume in a later segment.
    Suspended,
    /// The host declined to run this retry policy in one segment: its
    /// worst-case total backoff exceeds the tenant's host-retry budget.
    ///
    /// The call was NOT made, no event was recorded, and no attempt was
    /// consumed, so the caller may run the policy itself from attempt 1 --
    /// which is what `cleat_call_with_retry` does. Non-retryable as itself:
    /// re-issuing the same policy is refused on identical grounds.
    ///
    /// `callErrorCode` 6, `RetryPolicyTooLong` in `ABI.md`. The threshold used
    /// to be `HOST_RETRY_BUDGET_MS`, a constant compiled into this crate and
    /// held equal to Go's by a test that scraped one language's source from
    /// the other. It lives on the host now, per tenant -- IMPROVEMENT-PLAN
    /// 3.94 step 4.
    RetryPolicyTooLong,
    /// The host refused or failed the call.
    Failed(String),
}

impl std::fmt::Display for CallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CallError::Suspended => write!(f, "workflow suspended"),
            CallError::RetryPolicyTooLong => write!(
                f,
                "retry policy rejected: worst-case backoff exceeds the host-retry budget"
            ),
            CallError::Failed(msg) => write!(f, "{msg}"),
        }
    }
}

impl std::error::Error for CallError {}

/// Lets `?` carry a `CallError` into a workflow that returns
/// `Result<T, String>`, which most of them do.
///
/// # Why this does not lose the suspension
///
/// Flattening `Suspended` into a string looks like it should be dangerous: the
/// workflow now returns an *error* for something that is not one, and the
/// distinction the `CallError` enum exists to draw is gone by the time the
/// value reaches `#[cleat_entry]`.
///
/// It is safe because the wrapper does not consult the returned value to decide
/// suspension. `mark_suspended` was called by the host-call wrapper that
/// produced the error, and `#[cleat_entry]` checks that flag *before* it
/// formats the result at all -- so a suspended segment returns
/// `SUSPEND_SENTINEL` and this string is never written anywhere.
///
/// Which is to say: the flag is not redundant with the type, and this
/// conversion is the clearest demonstration of why. Removing the flag would
/// make this `impl` a silent bug.
impl From<CallError> for String {
    fn from(e: CallError) -> String {
        e.to_string()
    }
}

thread_local! {
    /// Set when a host call decides this segment suspends.
    ///
    /// This is the BACKSTOP, not the mechanism. The mechanism is
    /// `Result<_, CallError>` and `?`, which the compiler enforces: a workflow
    /// cannot use a value that was never produced. The flag exists for the one
    /// case the type system cannot reach -- a body that receives
    /// `Err(Suspended)` and discards it (`let _ = h.sleep_ms(..)`) and then
    /// returns a value of its own. Without it that body would report a normal
    /// result for a segment the host has already recorded as suspended, which
    /// is the exact failure the panic version had.
    static SUSPENDED: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

/// Records that this segment is suspending. Called by the host-call wrappers.
pub fn mark_suspended() {
    SUSPENDED.with(|s| s.set(true));
}

/// Reports whether anything in this segment asked to suspend.
pub fn is_suspended() -> bool {
    SUSPENDED.with(|s| s.get())
}

/// Clears the suspension flag.
///
/// Called by `#[cleat_entry]` before the body runs, not only after. A WASM
/// instance can serve more than one call, and a flag left set by a previous
/// segment would suspend the next one before it did anything.
pub fn clear_suspended() {
    SUSPENDED.with(|s| s.set(false));
}

/// Convert a Result<T, E> into a Result<String, String> for serialized export.
///
/// On `Ok`, the value is serialized to JSON. On `Err`, the error is converted
/// to its display string representation.
pub fn format_cleat_result<T: serde::Serialize, E: std::fmt::Display>(
    r: std::result::Result<T, E>,
) -> std::result::Result<String, String> {
    match r {
        Ok(val) => serde_json::to_string(&val).map_err(|e| format!("serialize workflow result: {}", e)),
        Err(e) => Err(e.to_string()),
    }
}
