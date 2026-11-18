//! The `schedules` group of host-call bindings (cleat#3827): `cleat_schedule_cron`, `cleat_delete_cron` and `cleat_list_crons`.
//!
//! NONE OF THE THREE WAITS, so none is a dispatch point and none goes through `dispatchThen`. The one way any of them can end a
//! segment is the host's refusal to start new work in a defer phase, which only `schedule_cron` is subject to (the engine puts
//! `stopBeforeNewWork` on it and on nothing else here; removing or reading a schedule is not new work) and which the SDK turns into
//! a suspension that `guarded` reports as one.
//!
//! ALL THREE ARE GUARDED, including the two reads. Each records an event (`schedule_cron`, `delete_cron`, `list_crons`), and
//! `list_crons` is journaled precisely because the set of schedules changes over time and a replay must not see a different
//! answer than the run did. A segment must record nothing past the call that suspended it.
//!
//! THE HOST'S MESSAGE IS PASSED THROUGH. A failed `schedule_cron` writes its reason (a bad expression, an unknown zone, input that
//! is not JSON, a quota, "no workflow store configured") into the buffer, and `HostCalls::schedule_cron` returns it; it is
//! forwarded unchanged and not replaced with a legend.

use javy_plugin_api::javy::quickjs::{Object, prelude::Func};
use serde_json::json;

use crate::guarded;
use cleat_sdk::HostCalls;

pub(crate) fn install(g: &Object<'_>) {
    g.set(
        "__cleat_schedule_cron",
        Func::from(
            |workflow_name: String,
             cron_expr: String,
             timezone: String,
             input_json: String|
             -> String {
                guarded(|| {
                    match HostCalls.schedule_cron(
                        &workflow_name,
                        &cron_expr,
                        &timezone,
                        &input_json,
                    ) {
                        Ok(id) => json!({ "ok": id }),
                        // The SDK reports the host's own text, or -- when the host refused before writing one -- a
                        // message naming the call and the host error code. cleat#4035 fixed that at the source, so the
                        // stand-in this used to call is gone: a workaround for a broken decoder is not a binding detail.
                        Err(e) => json!({ "err": e }),
                    }
                })
            },
        ),
    )
    .unwrap();

    g.set(
        "__cleat_delete_cron",
        Func::from(|schedule_id: String| -> String {
            guarded(|| match HostCalls.delete_cron(&schedule_id) {
                Ok(()) => json!({ "ok": null }),
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();

    // The host's JSON array, as TEXT: the TypeScript side parses it and renames its fields, so there is one place that knows the
    // wire shape.
    g.set(
        "__cleat_list_crons",
        Func::from(|| -> String {
            guarded(|| match HostCalls.list_crons() {
                Ok(list) => json!({ "ok": list }),
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();
}


