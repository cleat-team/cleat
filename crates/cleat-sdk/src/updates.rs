//! Workflow updates: a request/reply call into a running workflow that can
//! both change its state and return a value to the caller.
//!
//! # Why the handler registry lives here and not on `HostCalls`
//!
//! `HostCalls` is a unit struct with no state, so a registry cannot hang off
//! it. `thread_local!` is the same choice `defer` makes, and it is sound for
//! the same reason: a WASM guest is single-threaded, and one workflow segment
//! is one thread.
//!
//! # Why dispatch happens where it does
//!
//! An update handler is a closure in guest memory. Only guest code can call it,
//! so an arriving update cannot interrupt the workflow -- something in the
//! guest has to ask, and [`dispatch_updates`] is the asking.
//!
//! It has to ask at a fixed PROGRAM POSITION rather than a moment in time,
//! because replay re-executes the guest and matches host calls against the
//! recorded history in order. The SDK therefore dispatches immediately before
//! each suspension, and exports this for workflows that want more. See
//! `engine/updater.go` for the host side and why delivery is an event.
//!
//! The consequence, stated rather than hidden: an update is handled at the next
//! dispatch point, not the instant it arrives.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;

use crate::host_calls::HostCalls;

/// A registered update handler: receives the payload JSON, returns the result
/// JSON or a failure message.
type BoxedHandler = Box<dyn Fn(&str) -> Result<String, String>>;

/// A registered validator: returns Ok to accept, Err with a message to refuse.
type BoxedValidator = Box<dyn Fn(&str) -> Result<(), String>>;

/// One registered handler and its optional validator.
///
/// The validator runs first and is read-only, so a refusal costs nothing
/// beyond the completion -- no state change, no durable work. That is the half
/// of the API that makes an update different from a signal.
struct UpdateEntry {
    handler: BoxedHandler,
    validator: Option<BoxedValidator>,
}

thread_local! {
    static HANDLERS: RefCell<HashMap<String, UpdateEntry>> = RefCell::new(HashMap::new());

    /// Reentrancy guard. Every dispatch point is a suspension point, and an
    /// update handler is ordinary workflow code that may sleep or await -- so
    /// without this a handler doing either would re-enter dispatch and recurse.
    /// Nesting would also be wrong if it terminated: the inner dispatch would
    /// interleave a second update's events inside the first one's, and the
    /// received/completed pair would no longer bracket the handler that
    /// produced it.
    static DISPATCHING: Cell<bool> = const { Cell::new(false) };

    /// Set by a guest whose update handlers are not Rust closures (cleat-js-plugin: they are JavaScript functions, and
    /// asynchronous). The SDK's own dispatch points still run inside `sleep`, `await_signals`, `await_child`,
    /// `await_promise` and `wait_for_event`, and with no Rust handler registered they would POLL the host, find the
    /// update, and answer it "no update handler registered" before the guest's own dispatch ever saw it.
    static EXTERNAL_DISPATCH: Cell<bool> = const { Cell::new(false) };
}

/// Make the SDK's own dispatch points do nothing, because the guest dispatches updates itself.
///
/// For a language bridge only. A Rust workflow must never call this: its handlers are registered here, and with this set
/// they are never run. Nothing else changes: `poll_update`, `complete_update` and `register_update_handler` still work
/// and still record what they record.
#[doc(hidden)]
pub fn set_external_dispatch(on: bool) {
    EXTERNAL_DISPATCH.with(|e| e.set(on));
}

/// The envelope `cleat_poll_update` writes. Mirrors `engine/updater.go`'s
/// struct of the same name; the two are one wire format read from opposite
/// sides.
#[derive(Debug)]
pub(crate) struct UpdateDelivery {
    pub name: String,
    pub payload: String,
    pub request_id: String,
}

/// Parse the delivery envelope.
///
/// Hand-rolled rather than serde: this crate's other JSON boundaries are
/// hand-parsed too, and pulling serde in for three string fields would put a
/// dependency in every guest that merely suspends -- which, since every
/// suspension point is a dispatch point, is all of them.
pub(crate) fn parse_delivery(json: &str) -> Option<UpdateDelivery> {
    Some(UpdateDelivery {
        name: json_string_field(json, "name")?,
        payload: json_string_field(json, "payload")?,
        request_id: json_string_field(json, "request_id")?,
    })
}

fn json_string_field(json: &str, key: &str) -> Option<String> {
    let needle = format!("\"{}\":", key);
    let start = json.find(&needle)? + needle.len();
    let rest = &json[start..];
    let open = rest.find('"')? + 1;
    let bytes = rest.as_bytes();
    let mut out = String::new();
    let mut i = open;
    while i < bytes.len() {
        match bytes[i] {
            b'"' => return Some(out),
            b'\\' if i + 1 < bytes.len() => {
                i += 1;
                match bytes[i] {
                    b'n' => out.push('\n'),
                    b't' => out.push('\t'),
                    b'r' => out.push('\r'),
                    b'u' => {
                        // \uXXXX. The host writes these for control characters
                        // and, on the Go side only, for < > &.
                        let hex = rest.get(i + 1..i + 5)?;
                        let cp = u32::from_str_radix(hex, 16).ok()?;
                        out.push(char::from_u32(cp)?);
                        i += 4;
                    }
                    other => out.push(other as char),
                }
            }
            other => out.push(other as char),
        }
        i += 1;
    }
    None
}

impl HostCalls {
    /// Register an update handler with an optional validator.
    ///
    /// Mirrors Go's `RegisterUpdateHandler`. The name is registered with the
    /// host as well, which records it in the event history; the closure stays
    /// here, because only guest code can call it.
    pub fn register_update_handler_fn<H, V>(&self, name: &str, handler: H, validator: Option<V>)
    where
        H: Fn(&str) -> Result<String, String> + 'static,
        V: Fn(&str) -> Result<(), String> + 'static,
    {
        HANDLERS.with(|h| {
            h.borrow_mut().insert(
                name.to_string(),
                UpdateEntry {
                    handler: Box::new(handler),
                    validator: validator.map(|v| Box::new(v) as BoxedValidator),
                },
            );
        });
        self.register_update_handler(name);
    }

    /// Deliver and run every update currently pending for this workflow.
    ///
    /// The SDK already calls this before each suspension, so an ordinary
    /// workflow needs no update-specific code. See the module docs for why the
    /// position matters more than the timing.
    pub fn dispatch_updates(&self) {
        dispatch_with(
            || self.poll_update(),
            |request_id, result, err| {
                let _ = self.complete_update(request_id, result, err);
            },
        );
    }
}

/// The dispatch loop with the two host calls passed in, so that it can be driven natively: `poll_update` and
/// `complete_update` exist only in a WASM guest. `HostCalls::dispatch_updates` is this function with the real calls, and
/// nothing else.
fn dispatch_with(
    mut poll: impl FnMut() -> (String, bool, Option<String>),
    mut complete: impl FnMut(&str, &str, &str),
) {
    if EXTERNAL_DISPATCH.with(|e| e.get()) || DISPATCHING.with(|d| d.get()) {
        return;
    }
    DISPATCHING.with(|d| d.set(true));
    let _guard = DispatchGuard;

    loop {
        let (envelope, found, err) = poll();
        if err.is_some() || !found {
            return;
        }
        let delivery = match parse_delivery(&envelope) {
            Some(d) => d,
            // The envelope is written by the host, so this is not a caller
            // error. Returning rather than continuing avoids spinning on a
            // delivery that will decode the same way next time.
            None => return,
        };
        run_update_with(&delivery, &mut complete);
    }
}

/// Apply one delivered update and report the outcome.
///
/// Every path completes the request. A handler that is not registered, a
/// validator that refuses and a handler that errors are all answers the
/// caller is entitled to -- leaving any of them uncompleted would leave the
/// caller holding a promise nothing settles, which is the defect this
/// feature exists to end.
fn run_update_with(d: &UpdateDelivery, complete: &mut impl FnMut(&str, &str, &str)) {
    let outcome = HANDLERS.with(|h| {
        let handlers = h.borrow();
        let Some(entry) = handlers.get(&d.name) else {
            return Err(format!("cleat: no update handler registered for \"{}\"", d.name));
        };
        if let Some(validator) = &entry.validator {
            validator(&d.payload)?;
        }
        (entry.handler)(&d.payload)
    });

    match outcome {
        Ok(result) => complete(&d.request_id, &result, ""),
        Err(msg) => complete(&d.request_id, "", &msg),
    }
}

/// Clears the reentrancy flag however `dispatch_updates` leaves -- including
/// through the early returns, which is why this is a guard type rather than a
/// line at the end.
struct DispatchGuard;

impl Drop for DispatchGuard {
    fn drop(&mut self) {
        DISPATCHING.with(|d| d.set(false));
    }
}

/// Test-only: forget every registered handler.
///
/// Handlers live in thread-local state, so one test's registration is visible
/// to the next in the same thread. Nothing in a guest needs this -- a workflow
/// segment is a fresh instance.
#[doc(hidden)]
pub fn reset_handlers_for_test() {
    HANDLERS.with(|h| h.borrow_mut().clear());
    DISPATCHING.with(|d| d.set(false));
    EXTERNAL_DISPATCH.with(|e| e.set(false));
}

#[cfg(test)]
mod tests {
    use super::*;

    // The envelope decoder is the one part of the Rust update path testable
    // without a host: dispatch itself needs cleat_poll_update, which exists
    // only in a WASM guest.
    //
    // It is hand-rolled rather than serde, so it needs its own tests -- and the
    // cross-SDK case matters. Go's encoding/json escapes < > & as unicode
    // escapes while serde_json, json.dumps and Java's escapeJson do not, so the
    // same logical envelope reaches this decoder in two byte forms depending on
    // which side wrote it. Both must decode identically. That property was
    // asserted nowhere when the signal envelope had the same split
    // (IMPROVEMENT-PLAN 3.220).

    #[test]
    fn decodes_a_plain_envelope() {
        let d = parse_delivery(
            r#"{"name":"add","payload":"{\"n\":5}","request_id":"add\u0000prom-1"}"#,
        )
        .expect("should decode");
        assert_eq!(d.name, "add");
        assert_eq!(d.payload, r#"{"n":5}"#);
        assert_eq!(d.request_id, "add\0prom-1");
    }

    #[test]
    fn decodes_what_go_encodes() {
        // The raw strings below hold LITERAL escape text, not the characters:
        // go_form contains the six bytes \u003c where serde_form contains one
        // '<'. A decoder handling only \n \t \r would return "a \u003c b"
        // verbatim and hand the handler a payload no JSON parser accepts.
        let go_form = r#"{"name":"cmp","payload":"a \u003c b","request_id":"cmp\u0000p1"}"#;
        let serde_form = r#"{"name":"cmp","payload":"a < b","request_id":"cmp\u0000p1"}"#;
        assert_ne!(
            go_form, serde_form,
            "the two encodings must differ as BYTES, or this test asserts nothing"
        );

        let a = parse_delivery(go_form).expect("go form should decode");
        let b = parse_delivery(serde_form).expect("serde form should decode");
        assert_eq!(
            a.payload, b.payload,
            "the same envelope written by Go and by serde must decode alike"
        );
        assert_eq!(a.payload, "a < b");
    }

    #[test]
    fn an_empty_payload_is_an_outcome_not_a_failure() {
        // An update whose payload is "" must decode as "" rather than fail, or
        // a handler taking no arguments could never be called.
        let d = parse_delivery(r#"{"name":"ping","payload":"","request_id":"ping\u0000"}"#)
            .expect("should decode");
        assert_eq!(d.payload, "");
    }

    // What can be pinned natively is the switch's lifecycle: dispatch itself needs cleat_poll_update, which exists only
    // in a WASM guest. That `dispatch_updates` returns early when it is set is held by the TypeScript end-to-end test, which
    // fails with "no update handler registered" when this line in the plugin is removed.
    #[test]
    fn the_external_dispatch_switch_round_trips_and_is_reset_by_the_test_reset() {
        set_external_dispatch(true);
        assert!(EXTERNAL_DISPATCH.with(|e| e.get()));
        reset_handlers_for_test();
        assert!(!EXTERNAL_DISPATCH.with(|e| e.get()), "the test reset must clear the switch, or one test's setting leaks into the next");
        set_external_dispatch(true);
        set_external_dispatch(false);
        assert!(!EXTERNAL_DISPATCH.with(|e| e.get()));
    }

    // ---- the dispatch loop, driven natively through the seam -------------------------------------------------------------
    //
    // `dispatch_with` is what `HostCalls::dispatch_updates` runs with the real host calls, so these show a RUST handler
    // behaving as it always has, and that the external-dispatch switch (cleat#3827) changes nothing while it is unset.

    use std::cell::{Cell, RefCell};
    use std::rc::Rc;

    fn delivery(name: &str, payload: &str, id: &str) -> (String, bool, Option<String>) {
        (format!(r#"{{"name":"{name}","payload":{payload:?},"request_id":"{id}"}}"#), true, None)
    }

    /// Run the loop over `pending` deliveries (oldest first), and return what was completed as (id, result, error).
    fn run(pending: Vec<(String, bool, Option<String>)>) -> (Vec<(String, String, String)>, usize) {
        let queue = RefCell::new(pending.into_iter());
        let polls = Cell::new(0usize);
        let done = RefCell::new(Vec::new());
        dispatch_with(
            || {
                polls.set(polls.get() + 1);
                queue.borrow_mut().next().unwrap_or((String::new(), false, None))
            },
            |id, result, err| done.borrow_mut().push((id.to_string(), result.to_string(), err.to_string())),
        );
        (done.into_inner(), polls.get())
    }

    fn register(name: &str, handler: impl Fn(&str) -> Result<String, String> + 'static, validator: Option<BoxedValidator>) {
        HANDLERS.with(|h| {
            h.borrow_mut().insert(name.to_string(), UpdateEntry { handler: Box::new(handler), validator });
        });
    }

    #[test]
    fn a_rust_handler_runs_and_completes_with_its_result_when_the_switch_is_unset() {
        reset_handlers_for_test();
        register("add", |p| Ok(format!("<{p}>")), None);
        let (done, polls) = run(vec![delivery("add", "5", "r1"), delivery("add", "7", "r2")]);
        assert_eq!(done, vec![("r1".into(), "<5>".into(), "".into()), ("r2".into(), "<7>".into(), "".into())]);
        assert_eq!(polls, 3, "the loop polls until the host reports nothing pending: two deliveries and the empty one");
    }

    #[test]
    fn a_refusing_validator_and_an_unregistered_name_both_complete_with_an_error() {
        reset_handlers_for_test();
        let ran = Rc::new(Cell::new(0));
        let r = ran.clone();
        register(
            "guarded",
            move |_| {
                r.set(r.get() + 1);
                Ok("ran".into())
            },
            Some(Box::new(|_| Err("refused".to_string()))),
        );
        let (done, _) = run(vec![delivery("guarded", "1", "a"), delivery("missing", "1", "b")]);
        assert_eq!(ran.get(), 0, "a handler whose validator refused must not run");
        assert_eq!(done[0], ("a".into(), "".into(), "refused".into()));
        assert_eq!(done[1].2, r#"cleat: no update handler registered for "missing""#);
    }

    #[test]
    fn a_host_error_or_a_malformed_envelope_ends_the_loop_without_completing_anything() {
        reset_handlers_for_test();
        register("add", |_| Ok("x".into()), None);
        assert!(run(vec![(String::new(), false, Some("host refused".into()))]).0.is_empty());
        assert!(run(vec![(r#"{"name":"add","payload":"{}"}"#.to_string(), true, None)]).0.is_empty());
    }

    #[test]
    fn dispatch_is_not_reentrant() {
        reset_handlers_for_test();
        let inner_polls = Rc::new(Cell::new(0));
        let ip = inner_polls.clone();
        register(
            "outer",
            move |_| {
                // A handler that itself reaches a dispatch point: the inner dispatch must do nothing.
                dispatch_with(
                    || {
                        ip.set(ip.get() + 1);
                        (String::new(), false, None)
                    },
                    |_, _, _| {},
                );
                Ok("outer done".into())
            },
            None,
        );
        let (done, _) = run(vec![delivery("outer", "1", "o1")]);
        assert_eq!(done, vec![("o1".into(), "outer done".into(), "".into())]);
        assert_eq!(inner_polls.get(), 0, "the nested dispatch polled the host; the re-entrancy guard is gone");
    }

    #[test]
    fn with_the_switch_set_the_loop_neither_polls_nor_runs_a_rust_handler() {
        reset_handlers_for_test();
        let ran = Rc::new(Cell::new(0));
        let r = ran.clone();
        register(
            "add",
            move |_| {
                r.set(r.get() + 1);
                Ok("x".into())
            },
            None,
        );
        set_external_dispatch(true);
        let (done, polls) = run(vec![delivery("add", "1", "r1")]);
        assert_eq!((done.len(), polls, ran.get()), (0, 0, 0), "the switch must stop the loop before it asks the host");
        // And unsetting it restores the ordinary behaviour, on the same registered handler.
        set_external_dispatch(false);
        let (done, _) = run(vec![delivery("add", "1", "r1")]);
        assert_eq!(done.len(), 1);
        assert_eq!(ran.get(), 1);
    }

    #[test]
    fn a_malformed_envelope_is_rejected_rather_than_half_read() {
        // Missing request_id. Returning a delivery with an empty request id
        // would make complete_update address nothing, so the caller's promise
        // would never settle and the update would redeliver every segment.
        assert!(parse_delivery(r#"{"name":"add","payload":"{}"}"#).is_none());
    }
}
