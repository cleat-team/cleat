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

use crate::CallError;
use std::cell::RefCell;

/// A registered defer: the host's ID, and the body to run.
///
/// The body returns `Result<(), CallError>` rather than `()`, and that is the
/// defer half of IMPROVEMENT-PLAN 3.87. It used to be `Box<dyn FnOnce()>`, with
/// two things riding on unwinding that `wasm32-wasip1` does not have:
///
///   * a body that SUSPENDED did so by panicking with the old `SuspendSentinel`,
///     and `run_deferred` re-raised it so `#[cleat_entry]` could see it;
///   * a body that FAILED was isolated by `catch_unwind` so the remaining
///     bodies still ran.
///
/// `panic=abort` means neither worked in a shipped guest: the first body to
/// panic aborted the instance, taking the other defers with it. Both are now
/// return values -- `Err(CallError::Suspended)` and `Err(CallError::Failed)` --
/// which is the only form that survives on this target.
///
/// **A defer body must still not `panic!`.** Nothing can catch it here; the
/// guest aborts. That is not a decision this SDK gets to make, and no amount of
/// care in `run_deferred` changes it -- return `Err` instead.
type DeferEntry = (String, Box<dyn FnOnce() -> Result<(), CallError>>);

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
pub fn register_defer(defer_id: String, f: Box<dyn FnOnce() -> Result<(), CallError>>) {
    DEFERS.with(|d| d.borrow_mut().push((defer_id, f)));
}

thread_local! {
    /// True while `run_deferred` is draining the table.
    ///
    /// IMPROVEMENT-PLAN §3.35 phase 4. Two things a defer body must not do,
    /// both measured 2026-09-02 before they were blocked, and both producing a
    /// workflow that reported SUCCESS with a durable record that could not be
    /// honoured:
    ///
    /// * registering another defer — the table is drained BEFORE the first body
    ///   runs, so the new entry lands in a table nobody walks again, while the
    ///   host has already written a durable `defer` event for it;
    /// * `continue_as_new` — the host records the event AND the wrapper reports
    ///   the already-decided result, so the worker stores `done` and the
    ///   continuation is silently never taken.
    ///
    /// Both guards run BEFORE the host call. Checking after would leave the
    /// durable event behind, which is the defect rather than the fix.
    static IN_DEFER_PHASE: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

/// Reports whether defer bodies are currently running.
pub fn in_defer_phase() -> bool {
    IN_DEFER_PHASE.with(|f| f.get())
}

/// The message both refusals carry.
pub fn defer_phase_refusal(what: &str) -> String {
    format!(
        "cleat: {what} is not allowed from inside a defer body: the defer table is \
         drained before the first body runs and the workflow's result is already \
         decided, so this would be recorded durably and never taken \
         (IMPROVEMENT-PLAN 3.35 phase 4)"
    )
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
/// A body that returns `Err(CallError::Failed)` does not stop the others and
/// does not disturb the workflow's result, which is already decided by the time
/// this runs. `Err(CallError::Suspended)` is different: it is not an error, and
/// `crate::mark_suspended` has already been set by the host-call wrapper that
/// produced it, so `#[cleat_entry]` suspends the segment. Treating it as a
/// failure here would complete a workflow the host has already recorded as
/// suspended.
///
/// The remaining bodies still run after a suspending one. That is deliberate
/// and worth stating, because the alternative is defensible: stopping would
/// leave the un-run bodies in a table that has ALREADY been drained, so they
/// would never run at all, and their cleanup would be silently dropped. Running
/// them is the lesser of the two.
pub fn run_deferred() -> i64 {
    let taken: Vec<DeferEntry> = DEFERS.with(|d| d.borrow_mut().drain(..).collect());
    let mut ran = 0i64;

    // Set for the duration of the drain and cleared on EVERY exit -- a flag
    // left set would make the next segment's first defer_func refuse. Kept as a
    // guard struct rather than a pair of assignments: it costs nothing, and an
    // early return added to the loop later would otherwise leak the flag.
    struct PhaseGuard;
    impl Drop for PhaseGuard {
        fn drop(&mut self) {
            IN_DEFER_PHASE.with(|f| f.set(false));
        }
    }
    IN_DEFER_PHASE.with(|f| f.set(true));
    let _phase = PhaseGuard;

    for (_id, f) in taken.into_iter().rev() {
        ran += 1;
        // A failing body is isolated by ignoring its Err, which is all the
        // isolation this target allows: `catch_unwind` cannot help under
        // panic=abort. Suspension needs nothing here either -- the wrapper that
        // returned it already called mark_suspended, and #[cleat_entry] reads
        // that flag after this returns.
        let _ = f();
    }
    ran
}

/// Lets the HOST drain the defer table, for workflows that never reach the
/// `#[cleat_entry]` wrapper that normally does it.
///
/// Safe to call on a guest that has already run its defers: the table is
/// drained, so a second call runs nothing and returns 0.
///
/// A suspension raised by a body is discarded here, where `#[cleat_entry]`
/// acts on it. That difference is deliberate and survives the 3.87 rewrite
/// unchanged in meaning: the wrapper needs to know so its segment suspends, but
/// a workflow reached through this export is already dead and has no segment
/// left to suspend. Leaving the flag set would make the host's cleanup call
/// look like a suspension of a workflow that has already finished.
///
/// The name cannot collide with a workflow entry point: `#[cleat_entry]`
/// exports use the Rust function's own name, and a leading double underscore is
/// reserved by convention here.
#[no_mangle]
pub extern "C" fn __cleat_run_deferred() -> i64 {
    let ran = run_deferred();
    crate::clear_suspended();
    ran
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::rc::Rc;

    /// Each test drains first, because DEFERS is a thread-local shared by every
    /// test on this thread. Without it a leftover body from another test runs
    /// here and the counts below are someone else's.
    ///
    /// The suspension flag is cleared for the same reason, and it matters more:
    /// it is global to the thread, so a test that suspends would otherwise
    /// leave every later test looking suspended.
    fn fresh() {
        DEFERS.with(|d| d.borrow_mut().clear());
        crate::clear_suspended();
    }

    /// A body that does its work and succeeds.
    fn ok(f: impl FnOnce() + 'static) -> Box<dyn FnOnce() -> Result<(), CallError>> {
        Box::new(move || {
            f();
            Ok(())
        })
    }

    #[test]
    fn runs_bodies_in_lifo_order() {
        fresh();
        let order = Rc::new(RefCell::new(Vec::new()));
        for name in ["first", "second", "third"] {
            let o = Rc::clone(&order);
            register_defer(name.to_string(), ok(move || o.borrow_mut().push(name)));
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
        register_defer("x".into(), ok(move || *r.borrow_mut() += 1));

        assert_eq!(run_deferred(), 1);
        assert_eq!(run_deferred(), 0, "the table was not drained");
        assert_eq!(*runs.borrow(), 1, "cleanup ran twice; a lock would be released twice");
    }

    /// IMPROVEMENT-PLAN 3.87. This was `a_panicking_body_does_not_stop_the_others`
    /// and it tested something the shipped guest could not do.
    ///
    /// A panicking body was isolated with `catch_unwind`, which works on the
    /// HOST target this test runs on and never worked on `wasm32-wasip1`, where
    /// `panic=abort` turns the first panicking defer into an aborted instance
    /// and the remaining bodies never run at all. The test passed and the
    /// property did not hold where it mattered.
    ///
    /// Failure is a return value now, which behaves the same on both targets.
    #[test]
    fn a_failing_body_does_not_stop_the_others() {
        fresh();
        let ran = Rc::new(RefCell::new(Vec::new()));
        let a = Rc::clone(&ran);
        register_defer("a".into(), ok(move || a.borrow_mut().push("a")));
        register_defer(
            "boom".into(),
            Box::new(|| Err(CallError::Failed("cleanup did not work".into()))),
        );
        let c = Rc::clone(&ran);
        register_defer("c".into(), ok(move || c.borrow_mut().push("c")));

        assert_eq!(run_deferred(), 3);
        // "c" is registered last so it runs first; "a" must still run after the
        // failing one between them.
        assert_eq!(*ran.borrow(), vec!["c", "a"]);
    }

    /// The two callers need OPPOSITE behaviour for a suspending defer, and this
    /// is the pair that pins the difference. `run_deferred` must leave the
    /// suspension VISIBLE so the `#[cleat_entry]` wrapper suspends the segment;
    /// clearing it there completes a workflow the host has already recorded as
    /// suspended.
    #[test]
    fn run_deferred_leaves_a_suspension_visible() {
        fresh();
        register_defer("suspends".into(), Box::new(|| {
            crate::mark_suspended();
            Err(CallError::Suspended)
        }));

        assert_eq!(run_deferred(), 1);
        assert!(
            crate::is_suspended(),
            "run_deferred hid the suspension. The entry point wrapper never \
             learns the segment suspended, so a suspended workflow is reported \
             as complete."
        );
        crate::clear_suspended();
    }

    /// ...and the host's export must clear it. A workflow reached through
    /// __cleat_run_deferred was killed and has no segment left to suspend, so a
    /// suspension left visible would make the host's cleanup call on a dead
    /// workflow look like a suspension.
    #[test]
    fn the_host_export_clears_a_suspension() {
        fresh();
        register_defer("suspends".into(), Box::new(|| {
            crate::mark_suspended();
            Err(CallError::Suspended)
        }));

        assert_eq!(__cleat_run_deferred(), 1);
        assert!(
            !crate::is_suspended(),
            "a suspension escaped __cleat_run_deferred and would report a \
             workflow that is already dead as merely suspended"
        );
    }

    /// A body that suspends must not stop the bodies after it.
    ///
    /// The table is drained BEFORE the first body runs, so a body skipped here
    /// is never run by anyone -- there is no table left for a later segment to
    /// walk. Stopping would silently drop that cleanup, which is worse than
    /// running it in a segment that is about to suspend.
    #[test]
    fn a_suspending_body_does_not_stop_the_others() {
        fresh();
        let ran = Rc::new(RefCell::new(Vec::new()));
        let a = Rc::clone(&ran);
        register_defer("a".into(), ok(move || a.borrow_mut().push("a")));
        register_defer("suspends".into(), Box::new(|| {
            crate::mark_suspended();
            Err(CallError::Suspended)
        }));
        let c = Rc::clone(&ran);
        register_defer("c".into(), ok(move || c.borrow_mut().push("c")));

        assert_eq!(run_deferred(), 3);
        assert_eq!(*ran.borrow(), vec!["c", "a"]);
        assert!(crate::is_suspended());
        crate::clear_suspended();
    }

    #[test]
    fn the_host_export_is_safe_on_an_empty_table() {
        fresh();
        assert_eq!(__cleat_run_deferred(), 0);
    }
}

#[cfg(test)]
mod phase_tests {
    use super::*;
    use std::cell::RefCell;

    thread_local! {
        static LOG: RefCell<Vec<&'static str>> = const { RefCell::new(Vec::new()) };
    }

    /// IMPROVEMENT-PLAN 3.35 phase 4: the flag is set while bodies run.
    ///
    /// Asserted from inside a body rather than from outside, because outside is
    /// exactly where it is always false and a test that checked there would
    /// pass against a flag that is never set at all.
    #[test]
    fn in_defer_phase_is_true_while_a_body_runs() {
        LOG.with(|l| l.borrow_mut().clear());
        DEFERS.with(|d| d.borrow_mut().clear());
        register_defer(
            "d1".to_string(),
            Box::new(|| {
                LOG.with(|l| {
                    l.borrow_mut()
                        .push(if in_defer_phase() { "inside" } else { "NOT-inside" })
                });
                Ok(())
            }),
        );
        assert!(!in_defer_phase(), "the flag must be clear before the drain");
        assert_eq!(run_deferred(), 1);
        LOG.with(|l| assert_eq!(l.borrow().as_slice(), &["inside"]));
        assert!(
            !in_defer_phase(),
            "the flag must be clear after the drain, or the next segment's \
             first defer_func would be refused"
        );
    }

    /// The guard clears the flag even when a body fails, which is the case a
    /// pair of plain assignments around the loop would get wrong.
    #[test]
    fn the_flag_is_cleared_even_when_a_body_fails() {
        DEFERS.with(|d| d.borrow_mut().clear());
        register_defer(
            "d1".to_string(),
            Box::new(|| Err(CallError::Failed("cleanup did not work".into()))),
        );
        assert_eq!(run_deferred(), 1);
        assert!(!in_defer_phase());
    }
}
