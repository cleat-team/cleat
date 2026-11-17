//! The `plugins` group of host-call bindings (cleat#3827 group I): `plugin_call`, `plugin_call_streaming` and
//! `cleat_wait_for_event`.
//!
//! `wait_for_event` shares the registry rather than the shape: `engine/wait_for_event.go` runs a claim loop that calls
//! `s.PluginCall` on every attempt, so the same scripted function answers it. Conventions:
//! `packages/cleat-ts/groups/README.md`.

use cleat_sdk::HostCalls;
use javy_plugin_api::javy::quickjs::{prelude::Func, Object};

use crate::{guarded, pair};

pub(crate) fn install(g: &Object<'_>) {
    // plugin_call and plugin_call_streaming are the two unprefixed exports, so their globals cannot follow the
    // `__cleat_<export>` spelling the rest of the tree uses; the export is named in the inventory file instead.
    g.set(
        "__cleat_plugin_call",
        Func::from(
            |plugin: String, function: String, input: String| -> String {
                guarded(|| pair(HostCalls.plugin_call(&plugin, &function, &input)))
            },
        ),
    )
    .unwrap();

    g.set(
        "__cleat_plugin_call_streaming",
        Func::from(
            |plugin: String, function: String, input: String| -> String {
                guarded(|| pair(HostCalls.plugin_call_streaming(&plugin, &function, &input)))
            },
        ),
    )
    .unwrap();

    // timeoutMs is a JS number, so it arrives as f64 and is truncated toward zero; the host takes an i64. The signal
    // names arrive as a JSON array string because that is what the ABI carries, and the SDK wrapper wants `&[&str]`,
    // so the array is parsed here rather than handed to it as text.
    //
    // Guarded like the other two, and for a sharper reason: the engine's claim loop makes a FRESH plugin call on its
    // first attempt, so an unguarded wait records a plugin_call past a suspension exactly as an unguarded plugin_call
    // would.
    //
    // THIS GUARD IS CURRENTLY UNREACHABLE THROUGH `ctx.waitForEvent`, and it stays anyway. That path goes through
    // `dispatchThen` (groups/plugins.ts), whose update poll is itself guarded: once a segment has suspended, the poll
    // answers "suspended" and the call is never made, so removing this guard changes nothing a workflow can observe
    // today. Measured by cleat-review on #4037 -- a late call with this guard removed still passes. It is kept because
    // the export is reachable without the TypeScript wrapper, and because the invariant belongs to the binding: a future
    // caller that does not go through `dispatchThen` would get the unguarded behaviour this prevents. The absence of a
    // test that can see it is recorded here rather than papered over with one that passes for another reason.
    g.set(
        "__cleat_wait_for_event",
        Func::from(
            |plugin: String,
             function: String,
             input: String,
             names_json: String,
             timeout_ms: f64|
             -> String {
                guarded(|| {
                    let names: Vec<String> = serde_json::from_str(&names_json).unwrap_or_default();
                    let refs: Vec<&str> = names.iter().map(String::as_str).collect();
                    match HostCalls.wait_for_event_ms(
                        &plugin,
                        &function,
                        &input,
                        &refs,
                        timeout_ms as i64,
                    ) {
                        Ok(v) => pair((v, None)),
                        Err(e) => pair((String::new(), Some(e.to_string()))),
                    }
                })
            },
        ),
    )
    .unwrap();
}
