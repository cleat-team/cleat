//! The `detached` group of host-call bindings (cleat#3827): start a workflow nobody waits for.
//!
//! Two calls for one capability, and the second is not a replacement for the first. `cleat_run_detached` is what every deployed
//! binary imports with four parameters, and a host call's arity is part of its import type, so widening it stops those modules
//! INSTANTIATING rather than failing one call (engine/children.go, RunDetached's comment). `cleat_start_detached` arrived as a
//! new name for the same work, handing back the run id.
//!
//! BOTH GO THROUGH `guarded`, because both write: each records a `run_detached` event and starts a workflow. Neither PARKS the
//! workflow -- a genuine suspension is not among their answers -- so neither is a dispatch point; the one sentinel they can
//! return is the defer-phase refusal, which cleat-sdk reports as an error (see `HostCalls::run_detached`) rather than as a
//! suspension. That refusal reaches the guest as an `err` envelope here, which is the same shape every other failure takes.
//!
//! Conventions: `packages/cleat-ts/groups/README.md`.

use cleat_sdk::HostCalls;
use javy_plugin_api::javy::quickjs::Object;
use javy_plugin_api::javy::quickjs::prelude::Func;
use serde_json::json;

use crate::guarded;

pub(crate) fn install(g: &Object<'_>) {
    g.set(
        "__cleat_run_detached",
        Func::from(|name: String, input: String| -> String {
            guarded(|| match HostCalls.run_detached(&name, &input) {
                Ok(()) => json!({ "ok": null }),
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();
    g.set(
        "__cleat_start_detached",
        Func::from(|name: String, input: String| -> String {
            guarded(|| match HostCalls.start_detached(&name, &input) {
                Ok(run_id) => json!({ "ok": run_id }),
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();
}
