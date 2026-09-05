//! Every method on `cleat_sdk::HostCalls`, in one workflow, for coverage.
//!
//! This exists to be COMPILED, not run. Its job is to put the whole Rust
//! host-call surface in front of `rustc` and the linker, so a method that does
//! not exist, or whose signature drifted from what a workflow can pass it,
//! fails the build instead of failing a user.
//!
//! Why it is needed: `examples/rust-workflow` and the plugin-harness fixture
//! between them call 7 of 61 methods. The Go and Python SDKs had the same hole
//! and four Go host calls shipped uncompilable through it -- locks, promises
//! and side effects could not be built from any workflow. See
//! IMPROVEMENT-PLAN.md 3.204, 3.205 and 3.206.
//!
//! The calls live in `exercise_every_host_call`, which the entry point reaches
//! only when asked. `continue_as_new`, `release_lock` and friends would change
//! a running workflow's fate, and this file is on the compile path, not the
//! behaviour path.
//!
//! When adding a host call, add it here. `scripts/sdk-host-call-coverage.py`
//! reports the gap and fails when it grows.
//!
//! Compiled to WASM with: cargo build --target wasm32-wasip1 --release

use cleat_macro::cleat_entry;
use cleat_sdk::{ChildWorkflowOptions, HostCalls};
use std::time::Duration;

/// Calls every `HostCalls` method. Never invoked unless the input asks for it.
#[allow(clippy::let_underscore_untyped)]
fn exercise_every_host_call(h: &HostCalls) {
    let d = Duration::from_secs(1);

    // ---- durable calls ----
    let _ = h.call("svc", "op", "{}");
    let _ = h.cleat_call("svc", "op", "{}");
    let _ = h.cleat_call_heartbeat("svc", "op", "{}", 1000);
    let _ = h.cleat_send("svc", "op", "{}");
    let _ = h.schedule_invoke("svc", "op", "{}", d);
    let _ = h.schedule_invoke_ms("svc", "op", "{}", 1000);
    let _ = h.run_detached("wf", "{}");

    // ---- sleep, log, identity ----
    let _ = h.cleat_sleep(d);
    let _ = h.cleat_sleep_ms(1000);
    let _ = h.now();
    let _ = h.random();
    h.cleat_log("m");
    let _ = h.version();
    let _ = h.min_version();
    let _ = h.uuid("seed");
    let _ = h.workflow_id();
    let _ = h.run_id();

    // ---- scope ----
    let _ = h.set_scope("obj", "key");
    let _ = h.get_scope();
    let _ = h.clear_scope();

    // ---- defer, lifecycle ----
    let _ = h.cleat_defer("cleanup");
    let _ = h.continue_as_new("{}");
    let _ = h.continue_as_new_versioned("{}", 2);
    let _ = h.side_effect("computed");

    // ---- signals ----
    let _ = h.poll_cancellation();
    let _ = h.poll_signal("s");
    let _ = h.await_signals(&["s"], d);
    let _ = h.await_signals_ms(&["s"], 1000);
    let _ = h.await_signals_with_quorum(&["s".to_string()], 1, 0, 1000);
    let _ = h.signal_workflow("run-1", "s", "{}");
    let _ = h.send_signal_and_wait("run-1", "s", "{}", d);
    let _ = h.send_signal_and_wait_ms("run-1", "s", "{}", 1000);
    let _ = h.reply_to_signal("corr", "{}");

    // ---- children ----
    let opts = ChildWorkflowOptions {
        version: 0,
        parent_close_policy: "abandon".to_string(),
        priority: 0,
    };
    let _ = h.child_workflow("child", "{}");
    let _ = h.child_workflow_with_options("child", "{}", &opts);
    let _ = h.await_child("run-1");
    let _ = h.await_all_children(&["run-1"]);
    let _ = h.await_any_child(&["run-1"]);
    let _ = h.poll_child("run-1");

    // ---- promises ----
    let _ = h.create_promise("p");
    let _ = h.await_promise("prom-1", d);
    let _ = h.await_promise_ms("prom-1", 1000);
    let _ = h.resolve_promise("prom-1", "v");
    let _ = h.reject_promise("prom-1", "e");

    // ---- state ----
    h.set_query_state("k", "v");
    let _ = h.set_state("k", "v");
    let _ = h.get_state("k");
    let _ = h.delete_state("k");
    let _ = h.incr_state("k", 1);
    let _ = h.has_state("k");
    let _ = h.list_state("pre");

    // ---- locks ----
    let _ = h.acquire_lock("k", d);
    let _ = h.acquire_lock_ms("k", 1000);
    let _ = h.release_lock("k");

    // ---- http ----
    let _ = h.cleat_fetch("GET", "http://x", "{}", "");
    let _ = h.fetch_get("http://x");

    // ---- plugins ----
    let _ = h.plugin_call("p", "f", "{}");
    let _ = h.plugin_call_streaming("p", "f", "{}");

    // ---- updates, json helpers ----
    h.register_update_handler("upd");
    let _ = h.json_parse("{}");
    let _ = h.json_stringify("\"v\"");
}

#[cleat_entry]
fn all_host_calls(h: &HostCalls, input: String) -> Result<String, String> {
    if input == "exercise" {
        exercise_every_host_call(h);
    }
    Ok("ok".to_string())
}
