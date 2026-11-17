//! The `signals` group of host-call bindings (cleat#3827): `cleat_poll_signal` and `cleat_signal_workflow`.
//!
//! NEITHER SUSPENDS in the way `await_signals` does, so neither is a dispatch point and neither goes through `dispatchThen`
//! (the update group's hook, which wraps calls that can park the workflow). `poll_signal` answers at once; `signal_workflow` is
//! fire-and-forget. The one way `signal_workflow` can end a segment is the host's refusal to start new work in a defer phase,
//! which `HostCalls::signal_workflow` turns into a suspension, and `guarded` below reports that as a suspension.
//!
//! `poll_signal` is a READ and is deliberately not guarded: `engine/signaller.go` records no event for it and consumes
//! nothing, so a poll after the segment has suspended leaves no trace in the history. Measured, not assumed:
//! `TestTypeScriptASignalPollAfterASuspensionRecordsNothing`. (Guarding it would not make that test fail, which is why the
//! test exists as a measurement of the claim and not as a falsification of a guard.) `signal_workflow` records a
//! `signal_received` event and delivers, so it IS effectful and IS guarded, with a late-call test that fails without it.

use javy_plugin_api::javy::quickjs::{Object, prelude::Func};
use serde_json::json;

use crate::guarded;
use cleat_sdk::HostCalls;

pub(crate) fn install(g: &Object<'_>) {
    // `{"ok": null}` when nothing is pending and `{"ok": {"payload": "<text>"}}` when something is, so a delivered signal with
    // an empty payload is distinguishable from none. An error is the host's refusal of the name.
    g.set(
        "__cleat_poll_signal",
        Func::from(|name: String| -> String {
            match HostCalls.poll_signal(&name) {
                (_, _, Some(e)) => json!({ "err": e }),
                (payload, true, None) => json!({ "ok": { "payload": payload } }),
                (_, false, None) => json!({ "ok": null }),
            }
            .to_string()
        }),
    )
    .unwrap();

    g.set(
        "__cleat_signal_workflow",
        Func::from(|target_run_id: String, name: String, payload: String| -> String {
            guarded(|| match HostCalls.signal_workflow(&target_run_id, &name, &payload) {
                Ok(()) => json!({ "ok": null }),
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();
}
