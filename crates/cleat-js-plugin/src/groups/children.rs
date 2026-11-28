//! The `children` group of host-call bindings (cleat#3827).
//!
//! Four calls, in two shapes. `cleat_child_workflow_with_options` and `cleat_poll_child` answer with a value or an error
//! string, so they go through `pair`. The two waits answer with a JSON document the host built, which is parsed here so the
//! envelope nests an object rather than a string — and both go through `guarded`, because both dispatch pending updates
//! before they block (see `cleat/runtime_children.go`: `AwaitAllChildren` and `AwaitAnyChild` each call
//! `h.DispatchUpdates()`; `PollChild` does not). A binding that reached the host after this segment had suspended would
//! record an event the run does not own.
//!
//! Conventions: `packages/cleat-ts/groups/README.md`.

use cleat_sdk::{ChildWorkflowOptions, HostCalls};
use javy_plugin_api::javy::quickjs::Object;
use javy_plugin_api::javy::quickjs::prelude::Func;
use serde_json::{Value, json};

use crate::{guarded, pair};

/// The host answers a wait with JSON TEXT; nest the parsed document so the envelope is `{"ok":{...}}` and not
/// `{"ok":"..."}`. A document that will not parse is an error rather than a silent `null`: the only way to get one is a
/// host that changed shape under us, and passing it through as a string would make the SDK's types lie.
fn document(r: Result<String, cleat_sdk::CallError>) -> Value {
    match r {
        Err(e) => json!({ "err": format!("{e:?}") }),
        Ok(text) => match serde_json::from_str::<Value>(&text) {
            Ok(v) => json!({ "ok": v }),
            Err(e) => json!({ "err": format!("the host's answer was not JSON: {e}") }),
        },
    }
}

/// `runIDsJSON` is a JSON array of run-id strings, in the caller's order. The ABI takes a JSON array rather than a
/// delimited string so a run id containing any character at all is unambiguous.
fn run_ids(json_text: &str) -> Result<Vec<String>, Value> {
    serde_json::from_str::<Vec<String>>(json_text)
        .map_err(|e| json!({ "err": format!("run ids were not a JSON array of strings: {e}") }))
}

fn refs(ids: &[String]) -> Vec<&str> {
    ids.iter().map(String::as_str).collect()
}

pub(crate) fn install(g: &Object<'_>) {
    g.set(
        "__cleat_child_workflow_with_options",
        Func::from(
            |name: String,
             input: String,
             version: f64,
             priority: f64,
             parent_close_policy: String|
             -> String {
                guarded(|| {
                    let opts = ChildWorkflowOptions {
                        version: version as i64,
                        // 0 is the host's "unset"; JS callers pass no priority and it lands here as 0.
                        priority: priority as i32,
                        parent_close_policy,
                    };
                    pair(HostCalls.child_workflow_with_options(&name, &input, &opts))
                })
            },
        ),
    )
    .unwrap();
    g.set(
        "__cleat_await_all_children",
        Func::from(|run_ids_json: String| -> String {
            guarded(|| match run_ids(&run_ids_json) {
                Err(envelope) => envelope,
                Ok(ids) => document(HostCalls.await_all_children(&refs(&ids))),
            })
        }),
    )
    .unwrap();
    g.set(
        "__cleat_await_any_child",
        Func::from(|run_ids_json: String| -> String {
            guarded(|| match run_ids(&run_ids_json) {
                Err(envelope) => envelope,
                Ok(ids) => document(HostCalls.await_any_child(&refs(&ids))),
            })
        }),
    )
    .unwrap();
    g.set(
        "__cleat_poll_child",
        Func::from(|run_id: String| -> String {
            guarded(|| match HostCalls.poll_child(&run_id) {
                (_, Some(e)) => json!({ "err": e }),
                (text, None) => match serde_json::from_str::<Value>(&text) {
                    Ok(v) => json!({ "ok": v }),
                    Err(e) => json!({ "err": format!("the host's answer was not JSON: {e}") }),
                },
            })
        }),
    )
    .unwrap();
}
