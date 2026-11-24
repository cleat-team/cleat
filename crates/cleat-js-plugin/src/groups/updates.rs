//! The `updates` group of host-call bindings (cleat#3827): `cleat_register_update_handler`, `cleat_poll_update` and
//! `cleat_complete_update`.
//!
//! WHERE THE DISPATCH LIVES. The handlers are JavaScript functions and are asynchronous (a handler may `await` a durable
//! call, and a suspension inside one is normal), so they cannot be run from inside a Rust closure the way `cleat-sdk` runs
//! its own: Rust cannot wait for a promise that only the JavaScript event loop settles. The dispatch loop therefore lives
//! in `packages/cleat-ts/groups/updates.ts`, and these three bindings are the whole of what it needs from the host.
//!
//! THE SDK'S OWN DISPATCH IS TURNED OFF, and that is not optional. `cleat-sdk`'s `sleep`, `await_signals`, `await_child`,
//! `await_promise` and `wait_for_event` each dispatch pending updates before they call the host, and this plugin calls
//! those very methods. With no Rust handler registered they would poll the host, find a pending update and answer it "no
//! update handler registered", so an update meant for a TypeScript handler would be rejected by the plugin. The JavaScript
//! dispatch runs first and normally leaves nothing for them, which is why this is easy to miss: it bites when a handler is
//! running (its own `sleep` skips the JavaScript dispatch but not the SDK's) and for any call site that is not wrapped.
//! `set_external_dispatch` makes those points do nothing.
//!
//! Effectful, so all three go through `guarded`: `poll_update` records the delivery event when it finds something,
//! `complete_update` records the outcome, `register_update_handler` records the registration. A segment must record
//! nothing past the call that suspended it.

use javy_plugin_api::javy::quickjs::{Object, prelude::Func};
use serde_json::{Value, json};

use crate::guarded;
use cleat_sdk::HostCalls;

pub(crate) fn install(g: &Object<'_>) {
    // At install, which runs once while the plugin is initialised; the flag is a plain static in linear memory, so it is
    // part of the snapshot every module built on this plugin starts from.
    cleat_sdk::updates::set_external_dispatch(true);

    g.set(
        "__cleat_register_update_handler",
        Func::from(|name: String| -> String {
            guarded(|| {
                HostCalls.register_update_handler(&name);
                json!({ "ok": null })
            })
        }),
    )
    .unwrap();

    // The answer is the delivery envelope as an object, `{"name":...,"payload":...,"request_id":...}`, or null when
    // nothing is pending. A host error is an error, and a malformed envelope is one too: the host writes it, so a caller
    // that cannot read it must hear that and not be told "nothing pending" (which would leave the update redelivered
    // every segment while the caller waits).
    g.set(
        "__cleat_poll_update",
        Func::from(|| -> String {
            guarded(|| match HostCalls.poll_update() {
                (_, _, Some(e)) => json!({ "err": e }),
                (_, false, None) => json!({ "ok": null }),
                (envelope, true, None) => match serde_json::from_str::<Value>(&envelope) {
                    Ok(v) => json!({ "ok": v }),
                    Err(e) => json!({ "err": format!("cleat: the host's update envelope is not JSON: {e}") }),
                },
            })
        }),
    )
    .unwrap();

    // An empty `err_msg` resolves the caller's promise with `result_json`; a non-empty one rejects it.
    g.set(
        "__cleat_complete_update",
        Func::from(
            |request_id: String, result_json: String, err_msg: String| -> String {
                guarded(
                    || match HostCalls.complete_update(&request_id, &result_json, &err_msg) {
                        Ok(()) => json!({ "ok": null }),
                        Err(e) => json!({ "err": e }),
                    },
                )
            },
        ),
    )
    .unwrap();
}
