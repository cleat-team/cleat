//! The `#[cleat_entry]` suspension backstop, tested where it is observable.
//!
//! IMPROVEMENT-PLAN 3.87. Suspension is a return value -- `Result<T, CallError>`
//! and `?` -- and the compiler enforces the early return: a workflow cannot use
//! a value that was never produced. The flag is the backstop for the one case
//! the type system cannot reach, a body that receives `Err(CallError::Suspended)`
//! and discards it:
//!
//! ```ignore
//! let _ = h.cleat_sleep_ms(60_000);   // <- thrown away
//! Ok(my_own_result)                   // <- and then a value of its own
//! ```
//!
//! Reporting that value would complete a workflow the host has already recorded
//! as suspended, which is precisely the failure the panic version had. The
//! wrapper therefore checks `cleat_sdk::is_suspended()` BEFORE it formats the
//! result, and returns `memory::SUSPEND_SENTINEL` instead.
//!
//! # Why this is here and not in engine/rust_suspend_test.go
//!
//! It was there first, and the test could not fail. Every suspending host call
//! sets `session.suspendErr`, and `engine/executor.go` then returns a
//! `SuspendResult` with an empty result string whatever the guest returned --
//! so through `Engine.Execute` a discarding body looks identical whether the
//! backstop works or not. Measured: with both flag checks deleted from
//! `#[cleat_entry]`, the Go-side assertion still passed.
//!
//! Calling the generated export directly is what makes the property visible:
//! the return value is the guest's own, before the host has a chance to mask
//! it.
//!
//! Note this runs on the HOST target, where unwinding exists. That is fine
//! *because nothing here relies on unwinding* -- which is the whole point of
//! 3.87. The mechanism under test is a flag and a branch, and it behaves
//! identically on both targets. `engine/rust_suspend_test.go`'s
//! `TestARustGuestSuspendsCleanly` is the one that must run against a real
//! `wasm32-wasip1` build, and it does.

use cleat_macro::cleat_entry;
use cleat_sdk::CallError;

// `HostCalls` is deliberately NOT imported. #[cleat_entry] validates the first
// parameter by NAME and then rewrites the inner signature to
// `h: &cleat_sdk::HostCalls`, so the type the user wrote is never resolved.
// Importing it would be an unused import, and finding that out is a small piece
// of evidence about what the macro actually does with that parameter.
#[allow(dead_code)]
type HostCalls = cleat_sdk::HostCalls;

/// A workflow that suspends and propagates it, the ordinary shape.
#[cleat_entry]
fn propagates(_h: &HostCalls, _input: serde_json::Value) -> Result<String, String> {
    cleat_sdk::mark_suspended();
    Err(CallError::Suspended)?;
    Ok("body value".to_string())
}

/// A workflow that suspends and THROWS THE ERROR AWAY.
#[cleat_entry]
fn discards(_h: &HostCalls, _input: serde_json::Value) -> Result<String, String> {
    let suspended: Result<(), CallError> = {
        cleat_sdk::mark_suspended();
        Err(CallError::Suspended)
    };
    let _ = suspended;
    Ok("body value".to_string())
}

/// A workflow that does not suspend at all -- the control.
#[cleat_entry]
fn completes(_h: &HostCalls, _input: serde_json::Value) -> Result<String, String> {
    Ok("body value".to_string())
}

/// Calls a generated export the way the host does, and returns its i64.
fn invoke(
    f: unsafe extern "C" fn(*const u8, u32, *mut u8, u32) -> i64,
    out: &mut [u8],
) -> i64 {
    let args = b"{}";
    unsafe { f(args.as_ptr(), args.len() as u32, out.as_mut_ptr(), out.len() as u32) }
}

#[test]
fn a_body_that_propagates_a_suspension_returns_the_sentinel() {
    cleat_sdk::clear_suspended();
    let mut out = vec![0u8; 4096];
    let r = invoke(propagates, &mut out);
    assert_eq!(
        r,
        cleat_sdk::memory::SUSPEND_SENTINEL,
        "a workflow that propagated Err(Suspended) did not suspend"
    );
    cleat_sdk::clear_suspended();
}

#[test]
fn a_body_that_discards_a_suspension_still_returns_the_sentinel() {
    cleat_sdk::clear_suspended();
    let mut out = vec![0u8; 4096];
    let r = invoke(discards, &mut out);

    assert_eq!(
        r,
        cleat_sdk::memory::SUSPEND_SENTINEL,
        "THE BACKSTOP IS GONE.\n\n\
         The body discarded Err(CallError::Suspended) and returned a value of \
         its own, and #[cleat_entry] reported that value -- so a workflow the \
         host has already recorded as suspended would ALSO be recorded as \
         complete. That is the exact failure the panic version of this \
         mechanism had (IMPROVEMENT-PLAN 3.87), reintroduced by the fix for it.\n\n\
         The guard is `if cleat_sdk::is_suspended()` in \
         crates/cleat-macro/src/entry.rs, checked BEFORE format_cleat_result."
    );

    let written = String::from_utf8_lossy(&out);
    assert!(
        !written.contains("body value"),
        "the body's value was written to the output buffer: {written}"
    );
    cleat_sdk::clear_suspended();
}

/// The control, and it is what makes the two above findings rather than a
/// wrapper that returns SUSPEND_SENTINEL unconditionally.
#[test]
fn a_body_that_does_not_suspend_returns_its_result() {
    cleat_sdk::clear_suspended();
    let mut out = vec![0u8; 4096];
    let r = invoke(completes, &mut out);

    assert_ne!(
        r,
        cleat_sdk::memory::SUSPEND_SENTINEL,
        "a workflow that never suspended was reported as suspended, so the two \
         tests above prove nothing: the wrapper suspends unconditionally."
    );
    let written = String::from_utf8_lossy(&out);
    assert!(
        written.contains("body value"),
        "the completing workflow's own result did not reach the output buffer: {written}"
    );
}

/// The flag is a thread-local and `cargo test` reuses threads, so a segment
/// that suspends must not leave the next one looking suspended.
///
/// `#[cleat_entry]` clears it before the body runs for exactly this reason. The
/// assertion is the completing workflow succeeding AFTER a suspending one on
/// the same thread -- which is the shape a WASM instance serving two calls has.
#[test]
fn a_suspension_does_not_leak_into_the_next_call() {
    cleat_sdk::clear_suspended();
    let mut out = vec![0u8; 4096];

    assert_eq!(invoke(discards, &mut out), cleat_sdk::memory::SUSPEND_SENTINEL);

    let mut out2 = vec![0u8; 4096];
    let r = invoke(completes, &mut out2);
    assert_ne!(
        r,
        cleat_sdk::memory::SUSPEND_SENTINEL,
        "the suspension flag leaked into the next call on this thread, so a \
         workflow that never suspended was reported as suspended. \
         #[cleat_entry] must clear_suspended() BEFORE the body runs, not only \
         after."
    );
}
