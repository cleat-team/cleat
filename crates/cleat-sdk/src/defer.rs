//! Guest-side defer registry.
//!
//! IMPROVEMENT-PLAN §3.70 for Go, §3.73 for the other SDKs. A defer body is a
//! closure in the memory of the instance that registered it. The host runs
//! defers by invoking an export, and until now the only thing `cleat_defer`
//! carried across the boundary was a *description* — so the Rust SDK could
//! register a defer and the host could record it, and nothing anywhere could
//! ever run it. `HostCalls::cleat_defer` still exists and still does exactly
//! that; `HostCalls::defer_func` is the one that has a body.
//!
//! So the guest runs its own, in the instance that holds the closures, at the
//! moment the entry point finishes. `#[cleat_entry]`'s wrapper calls
//! `run_deferred` on the success and error paths but NOT on suspension — a
//! suspended workflow has not exited, and firing its cleanup at the first sleep
//! would release locks a workflow that is about to continue still holds.
//!
//! `__cleat_run_deferred` is the same drain, exported for the host to call on a
//! workflow it KILLED — one stopped by the execution fence, the instruction
//! limit, or an unrecoverable runtime failure, which never reaches the wrapper.
//! See §3.35 phase 4 and engine/backend_wasmtime.go's runGuestDefersAfterKill.

use std::cell::RefCell;

/// A registered defer: the host's ID, and the body to run.
type DeferEntry = (String, Box<dyn FnOnce()>);

thread_local! {
    /// Registered defer bodies, in registration order. WASM guests are
    /// single-threaded, so a thread-local is a module-global here.
    static DEFERS: RefCell<Vec<DeferEntry>> = RefCell::new(Vec::new());
}

/// Records a defer body under the ID the host minted for it.
///
/// The ID is not decorative: it is the same one the host recorded in the
/// workflow's deferrals map, so a body keyed by anything else would run but
/// could never be correlated with what the host thinks it registered.
pub fn register_defer(defer_id: String, f: Box<dyn FnOnce()>) {
    DEFERS.with(|d| d.borrow_mut().push((defer_id, f)));
}

/// Runs registered defer bodies in LIFO order and returns how many ran.
///
/// The table is drained BEFORE the first body runs, which makes this
/// idempotent: a second call runs nothing. That matters because the host cannot
/// always tell whether the guest already ran its own defers, and cleanup that
/// runs twice releases a lock twice or refunds a charge twice.
///
/// LIFO is not cosmetic. A defer releases what the defer before it acquired, so
/// running them in registration order unwinds the workflow inside-out.
///
/// A panic in one body does not stop the others and does not disturb the
/// workflow's result, which is already decided by the time this runs. The one
/// exception is `SuspendSentinel`, which is not an error: it is resumed so the
/// entry point wrapper sees it and the segment suspends. Swallowing it here
/// would complete a workflow the host has already recorded as suspended.
pub fn run_deferred() -> i64 {
    let taken: Vec<DeferEntry> = DEFERS.with(|d| d.borrow_mut().drain(..).collect());
    let mut ran = 0i64;
    for (_id, f) in taken.into_iter().rev() {
        ran += 1;
        if let Err(panic_err) = std::panic::catch_unwind(std::panic::AssertUnwindSafe(f)) {
            if panic_err.downcast_ref::<crate::SuspendSentinel>().is_some() {
                std::panic::resume_unwind(panic_err);
            }
        }
    }
    ran
}

/// Lets the HOST drain the defer table, for workflows that never reach the
/// `#[cleat_entry]` wrapper that normally does it.
///
/// Safe to call on a guest that has already run its defers: the table is
/// drained, so a second call runs nothing and returns 0.
///
/// Every panic is swallowed here, INCLUDING `SuspendSentinel`, which
/// `run_deferred` deliberately lets through. That is right for this caller and
/// wrong for the other: the wrapper needs the sentinel so its segment suspends,
/// but a workflow reached through this export is already dead and has no
/// segment left. Letting it out would turn the host's cleanup call into a trap.
///
/// The name cannot collide with a workflow entry point: `#[cleat_entry]`
/// exports use the Rust function's own name, and a leading double underscore is
/// reserved by convention here.
#[no_mangle]
pub extern "C" fn __cleat_run_deferred() -> i64 {
    std::panic::catch_unwind(run_deferred).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::rc::Rc;

    /// Each test drains first, because DEFERS is a thread-local shared by every
    /// test on this thread. Without it a leftover body from another test runs
    /// here and the counts below are someone else's.
    fn fresh() {
        DEFERS.with(|d| d.borrow_mut().clear());
    }

    #[test]
    fn runs_bodies_in_lifo_order() {
        fresh();
        let order = Rc::new(RefCell::new(Vec::new()));
        for name in ["first", "second", "third"] {
            let o = Rc::clone(&order);
            register_defer(name.to_string(), Box::new(move || o.borrow_mut().push(name)));
        }

        assert_eq!(run_deferred(), 3);
        // LIFO, not registration order. A defer releases what the one before it
        // acquired, so FIFO unwinds the workflow inside-out.
        assert_eq!(*order.borrow(), vec!["third", "second", "first"]);
    }

    #[test]
    fn draining_makes_a_second_run_a_no_op() {
        fresh();
        let runs = Rc::new(RefCell::new(0));
        let r = Rc::clone(&runs);
        register_defer("x".into(), Box::new(move || *r.borrow_mut() += 1));

        assert_eq!(run_deferred(), 1);
        assert_eq!(run_deferred(), 0, "the table was not drained");
        assert_eq!(*runs.borrow(), 1, "cleanup ran twice; a lock would be released twice");
    }

    #[test]
    fn a_panicking_body_does_not_stop_the_others() {
        fresh();
        let ran = Rc::new(RefCell::new(Vec::new()));
        let a = Rc::clone(&ran);
        register_defer("a".into(), Box::new(move || a.borrow_mut().push("a")));
        register_defer("boom".into(), Box::new(|| panic!("cleanup blew up")));
        let c = Rc::clone(&ran);
        register_defer("c".into(), Box::new(move || c.borrow_mut().push("c")));

        assert_eq!(run_deferred(), 3);
        // "c" is registered last so it runs first; "a" must still run after the
        // panicking one between them.
        assert_eq!(*ran.borrow(), vec!["c", "a"]);
    }

    /// The two callers need OPPOSITE behaviour for a suspending defer, and
    /// this is the pair that pins the difference. `run_deferred` must let
    /// SuspendSentinel out so the `#[cleat_entry]` wrapper suspends the
    /// segment; swallowing it there completes a workflow the host has already
    /// recorded as suspended.
    #[test]
    fn run_deferred_lets_a_suspend_sentinel_through() {
        fresh();
        register_defer("suspends".into(), Box::new(|| {
            std::panic::panic_any(crate::SuspendSentinel);
        }));

        let escaped = std::panic::catch_unwind(run_deferred);
        match escaped {
            Err(e) if e.downcast_ref::<crate::SuspendSentinel>().is_some() => {}
            Err(_) => panic!("a different panic escaped; the sentinel was replaced"),
            Ok(_) => panic!(
                "run_deferred swallowed SuspendSentinel. The entry point wrapper \
                 never learns the segment suspended, so a suspended workflow is \
                 reported as complete."
            ),
        }
    }

    /// ...and the host's export must NOT let it out. A workflow reached through
    /// __cleat_run_deferred was killed and has no segment left to suspend, so an
    /// escaping sentinel would turn the host's cleanup call into a trap.
    #[test]
    fn the_host_export_swallows_a_suspend_sentinel() {
        fresh();
        register_defer("suspends".into(), Box::new(|| {
            std::panic::panic_any(crate::SuspendSentinel);
        }));

        let outcome = std::panic::catch_unwind(|| __cleat_run_deferred());
        assert!(
            outcome.is_ok(),
            "SuspendSentinel escaped __cleat_run_deferred and would trap the host's \
             cleanup call on a workflow that is already dead"
        );
    }

    #[test]
    fn the_host_export_is_safe_on_an_empty_table() {
        fresh();
        assert_eq!(__cleat_run_deferred(), 0);
    }
}
