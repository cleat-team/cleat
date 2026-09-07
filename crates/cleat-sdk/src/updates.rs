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

/// One registered handler and its optional validator.
///
/// The validator runs first and is read-only, so a refusal costs nothing
/// beyond the completion -- no state change, no durable work. That is the half
/// of the API that makes an update different from a signal.
struct UpdateEntry {
    handler: Box<dyn Fn(&str) -> Result<String, String>>,
    validator: Option<Box<dyn Fn(&str) -> Result<(), String>>>,
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
                    validator: validator.map(|v| Box::new(v) as Box<dyn Fn(&str) -> Result<(), String>>),
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
        if DISPATCHING.with(|d| d.get()) {
            return;
        }
        DISPATCHING.with(|d| d.set(true));
        let _guard = DispatchGuard;

        loop {
            let (envelope, found, err) = self.poll_update();
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
            self.run_update(&delivery);
        }
    }

    /// Apply one delivered update and report the outcome.
    ///
    /// Every path completes the request. A handler that is not registered, a
    /// validator that refuses and a handler that errors are all answers the
    /// caller is entitled to -- leaving any of them uncompleted would leave the
    /// caller holding a promise nothing settles, which is the defect this
    /// feature exists to end.
    fn run_update(&self, d: &UpdateDelivery) {
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
            Ok(result) => {
                let _ = self.complete_update(&d.request_id, &result, "");
            }
            Err(msg) => {
                let _ = self.complete_update(&d.request_id, "", &msg);
            }
        }
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

    #[test]
    fn a_malformed_envelope_is_rejected_rather_than_half_read() {
        // Missing request_id. Returning a delivery with an empty request id
        // would make complete_update address nothing, so the caller's promise
        // would never settle and the update would redeliver every segment.
        assert!(parse_delivery(r#"{"name":"add","payload":"{}"}"#).is_none());
    }
}
