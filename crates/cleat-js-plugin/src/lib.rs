//! cleat-js: a Javy plugin that binds cleat's host calls into QuickJS and bridges cleat's guest ABI.
//!
//! cleat's ABI (ABI.md section 1) is `entry(args_ptr, args_len, out_ptr, max_out_len) -> i64`, and a Javy-built module
//! can only export `() -> ()` functions. `cleat-js-link` generates, per entry point, a function of the ABI's type that
//! calls `cleat_set_io`, then Javy's export, then `cleat_take_result`. The JavaScript in between reads its input with
//! `__cleat_args()` and writes its result with `__cleat_result(json)` or `__cleat_fail(message)` (the SDK layer,
//! packages/cleat-ts, does both).
//!
//! SUSPENSION (plan section 4.4). The host-call bindings are `cleat-sdk`'s `HostCalls`, unchanged: they decode the
//! suspend sentinel and the defer-phase stop bit and set the SDK's suspended flag. Each binding here returns
//! `{"suspended":true}` when that flag is set, and the TypeScript layer turns that into a promise that never settles.
//! Nothing is thrown, so a user's `try`/`catch` cannot swallow a suspension and no `finally` runs. After the entry's
//! microtasks drain, `cleat_take_result` returns SUSPEND_SENTINEL if the flag is set. Once the flag is set, further
//! bindings short-circuit without calling the host, so a segment records nothing past the call that suspended it.

use std::cell::RefCell;

use cleat_sdk::HostCalls;
use javy_plugin_api::{
    Config, import_namespace,
    javy::{Runtime, quickjs::prelude::Func},
};
use serde_json::{Value, json};

// The namespace a dynamically linked module would import this plugin's exports from. Unused in static mode.
import_namespace!("cleat-js-plugin-v0");

const SUSPEND_SENTINEL: i64 = 1 << 62;
const SUSPENDED: &str = r#"{"suspended":true}"#;

/// Comma-separated shim names to leave OUT of this build (see shims.js). Empty in every shipped build; set only by the
/// determinism control builds (`CLEAT_SHIMS_OFF=random cargo build ...`), so each control removes exactly one shim.
const SHIMS_OFF: &str = match option_env!("CLEAT_SHIMS_OFF") {
    Some(v) => v,
    None => "",
};

/// A fault to inject, for a control build only (`CLEAT_FAULT=random-nonnumeric`). Empty in every shipped build. It exists so the
/// claim "the random shim fails loudly on a host answer it cannot read" is run against a host that actually gives one, rather
/// than asserted: no real host call returns a non-number here.
const FAULT: &str = match option_env!("CLEAT_FAULT") {
    Some(v) => v,
    None => "",
};

#[derive(Default)]
struct Io {
    args_ptr: u32,
    args_len: u32,
    out_ptr: u32,
    out_max: u32,
    /// Bytes written to the output buffer, and the ABI error code that goes with them.
    written: Option<(u32, u32)>,
}

thread_local! {
    static IO: RefCell<Io> = RefCell::new(Io::default());
}

/// Copies `s` into the host's output buffer, truncating to its capacity. Returns the bytes written.
fn write_out(io: &Io, s: &str) -> u32 {
    let bytes = s.as_bytes();
    let n = bytes.len().min(io.out_max as usize);
    // SAFETY: the host passed (out_ptr, out_max) as a writable region of this module's linear memory for the
    // duration of the call, and `n` is clamped to it.
    unsafe {
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), io.out_ptr as *mut u8, n);
    }
    n as u32
}

#[unsafe(export_name = "cleat_set_io")]
pub extern "C" fn cleat_set_io(args_ptr: u32, args_len: u32, out_ptr: u32, out_max: u32) {
    // A WASM instance can serve more than one call, and a flag left set by the previous segment would suspend this one
    // before it did anything (cleat-sdk's own #[cleat_entry] clears it for the same reason).
    cleat_sdk::clear_suspended();
    IO.with(|io| {
        *io.borrow_mut() = Io { args_ptr, args_len, out_ptr, out_max, written: None };
    });
}

/// The packed ABI result: error code in the low 32 bits, bytes written in the high 32; or SUSPEND_SENTINEL. A workflow
/// that wrote no result and did not suspend is an error, so "returned nothing" cannot be mistaken for an empty success.
#[unsafe(export_name = "cleat_take_result")]
pub extern "C" fn cleat_take_result() -> i64 {
    if cleat_sdk::is_suspended() {
        cleat_sdk::clear_suspended();
        IO.with(|io| io.borrow_mut().written = None);
        return SUSPEND_SENTINEL;
    }
    IO.with(|io| {
        let mut io = io.borrow_mut();
        let (len, code) = match io.written {
            Some(w) => w,
            None => (write_out(&io, r#"{"error":"the workflow returned no result"}"#), 1),
        };
        io.written = None;
        ((len as i64) << 32) | (code as i64)
    })
}

fn json_string(message: &str) -> String {
    Value::String(message.to_string()).to_string()
}

/// A binding's JSON envelope: `{"suspended":true}`, `{"err":"..."}`, or `{"ok":<value>}`. Short-circuits (the host is
/// not called) once this segment has suspended.
fn guarded(f: impl FnOnce() -> Value) -> String {
    if cleat_sdk::is_suspended() {
        return SUSPENDED.to_string();
    }
    let v = f();
    if cleat_sdk::is_suspended() {
        return SUSPENDED.to_string();
    }
    v.to_string()
}

fn pair(r: (String, Option<String>)) -> Value {
    match r {
        (_, Some(e)) => json!({ "err": e }),
        (v, None) => json!({ "ok": v }),
    }
}

fn modify_runtime(runtime: Runtime) -> Runtime {
    runtime.context().with(|ctx| {
        let g = ctx.globals();
        // ---- the ABI bridge ------------------------------------------------------------------------------------
        g.set(
            "__cleat_args",
            Func::from(|| -> String {
                IO.with(|io| {
                    let io = io.borrow();
                    // SAFETY: (args_ptr, args_len) is the input region the host wrote before the call.
                    let b = unsafe { std::slice::from_raw_parts(io.args_ptr as *const u8, io.args_len as usize) };
                    String::from_utf8_lossy(b).into_owned()
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_result",
            Func::from(|json: String| {
                IO.with(|io| {
                    let mut io = io.borrow_mut();
                    let n = write_out(&io, &json);
                    io.written = Some((n, 0));
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_fail",
            Func::from(|message: String| {
                IO.with(|io| {
                    let mut io = io.borrow_mut();
                    let n = write_out(&io, &format!(r#"{{"error":{}}}"#, json_string(&message)));
                    io.written = Some((n, 1));
                })
            }),
        )
        .unwrap();
        g.set("__cleat_suspended", Func::from(|| -> bool { cleat_sdk::is_suspended() })).unwrap();

        // ---- host calls (the subset bound so far; cleat#3827 binds the rest) ------------------------------------------------
        g.set(
            "__cleat_call",
            Func::from(|service: String, operation: String, request: String| -> String {
                guarded(|| pair(HostCalls.call(&service, &operation, &request)))
            }),
        )
        .unwrap();
        g.set(
            "__cleat_sleep",
            Func::from(|ms: f64| -> String {
                guarded(|| match HostCalls.cleat_sleep_ms(ms as i64) {
                    Ok(()) => json!({ "ok": null }),
                    Err(e) => json!({ "err": format!("{e:?}") }),
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_await_signals",
            Func::from(|names_json: String, timeout_ms: f64| -> String {
                guarded(|| {
                    let names: Vec<String> = serde_json::from_str(&names_json).unwrap_or_default();
                    let refs: Vec<&str> = names.iter().map(String::as_str).collect();
                    match HostCalls.await_signals_ms(&refs, timeout_ms as i64) {
                        Ok(s) => json!({ "ok": { "name": s.name, "payload": s.payload, "timedOut": s.timed_out, "replyTo": s.reply_to } }),
                        Err(e) => json!({ "err": format!("{e:?}") }),
                    }
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_child_workflow",
            Func::from(|name: String, input: String| -> String { guarded(|| pair(HostCalls.child_workflow(&name, &input))) }),
        )
        .unwrap();
        g.set(
            "__cleat_await_child",
            Func::from(|run_id: String| -> String {
                guarded(|| match HostCalls.await_child(&run_id) {
                    Ok(v) => json!({ "ok": v }),
                    Err(e) => json!({ "err": format!("{e:?}") }),
                })
            }),
        )
        .unwrap();
        g.set("__cleat_now", Func::from(|| -> f64 { HostCalls.now() as f64 })).unwrap();
        // The host's random value is a full i64; a JS number is not, so it crosses as a decimal string.
        g.set(
            "__cleat_random",
            Func::from(|| -> String {
                if FAULT == "random-nonnumeric" {
                    return SUSPENDED.to_string();
                }
                HostCalls.random().to_string()
            }),
        )
        .unwrap();
        g.set("__cleat_uuid", Func::from(|seed: String| -> String { HostCalls.uuid(&seed) })).unwrap();
        // DurableLog: a host call the Go twin makes four times. Short-circuited once this segment has suspended, like the others.
        g.set(
            "__cleat_log",
            Func::from(|message: String| {
                if !cleat_sdk::is_suspended() {
                    HostCalls.cleat_log(&message)
                }
            }),
        )
        .unwrap();
        // The host's canonical JSON (ABI 6.3, cleat_json_stringify): parse and re-encode through Go's encoding/json, so keys are
        // sorted and the byte form is the one every other language's SDK is meant to produce. Empty string = the host refused it
        // (invalid JSON), which the SDK turns into an error rather than passing the text through.
        g.set(
            "__cleat_json_stringify",
            Func::from(|text: String| -> String { HostCalls.json_stringify(&text).unwrap_or_default() }),
        )
        .unwrap();
        g.set("__cleat_workflow_id", Func::from(|| -> String { HostCalls.workflow_id() })).unwrap();
        g.set(
            "__cleat_set_query_state",
            Func::from(|key: String, value: String| {
                if !cleat_sdk::is_suspended() {
                    HostCalls.set_query_state(&key, &value)
                }
            }),
        )
        .unwrap();
        g.set(
            "__cleat_side_effect",
            Func::from(|computed: String| -> String {
                guarded(|| match HostCalls.side_effect(&computed) {
                    Ok(v) => json!({ "ok": v }),
                    Err(e) => json!({ "err": e }),
                })
            }),
        )
        .unwrap();
        // ---- workflow metadata, scope and cancellation (cleat#3827) ------------------------------------------------
        // Reads, so unguarded: they record nothing and cannot fail, and the plugin leaves its other reads (`now`,
        // `workflow_id`, `uuid`) unguarded for the same reason.
        g.set("__cleat_version", Func::from(|| -> f64 { HostCalls.version() as f64 })).unwrap();
        g.set("__cleat_min_version", Func::from(|| -> f64 { HostCalls.min_version() as f64 })).unwrap();
        g.set("__cleat_run_id", Func::from(|| -> String { HostCalls.run_id() })).unwrap();
        g.set(
            "__cleat_poll_cancellation",
            Func::from(|| -> String {
                let (cancelled, reason) = HostCalls.poll_cancellation();
                json!({ "cancelled": cancelled, "reason": reason }).to_string()
            }),
        )
        .unwrap();
        g.set(
            "__cleat_get_scope",
            Func::from(|| -> String {
                let (object_type, instance_key) = HostCalls.get_scope();
                json!({ "objectType": object_type, "instanceKey": instance_key }).to_string()
            }),
        )
        .unwrap();
        // Effectful, so it goes through `guarded` like every other effectful call: it records a scope acquisition, and a
        // segment must record nothing past the call that suspended it. Measured, not assumed (cleat#3827): with this
        // binding unguarded, a workflow that calls it after a `sleep` gets a scope_acquired event recorded for a scope the
        // run does not hold.
        //
        // The previous scope crosses as a PAIR, not as the host's `vo:<type>:<key>:` string, so this layer's two scope
        // calls agree with each other. A non-empty answer in a form this does not recognise is an ERROR rather than an
        // empty scope: silently emptying it would report "I held nothing" for a run that held something.
        g.set(
            "__cleat_set_scope",
            Func::from(|object_type: String, instance_key: String| -> String {
                guarded(|| match HostCalls.set_scope(&object_type, &instance_key) {
                    prev if prev.is_empty() => json!({ "ok": { "objectType": "", "instanceKey": "" } }),
                    prev => match prev.strip_prefix("vo:").and_then(|s| s.strip_suffix(':')).and_then(|s| s.split_once(':')) {
                        Some((t, k)) => json!({ "ok": { "objectType": t, "instanceKey": k } }),
                        None => json!({ "err": format!("the host reported a previous scope in a form this binding does not know: {prev}") }),
                    },
                })
            }),
        )
        .unwrap();
        // ---- durable promises (cleat#3827) ---------------------------------------------------------------------------
        // All four are effectful (create records the event that asserts the promise exists; settle records the outcome),
        // so all four are guarded. await_promise is the one that waits, and a suspension reaches TypeScript as a promise
        // that never settles, as with every other waiting call in this layer.
        g.set("__cleat_create_promise", Func::from(|name: String| -> String { guarded(|| pair(HostCalls.create_promise(&name))) })).unwrap();
        g.set(
            "__cleat_await_promise",
            Func::from(|promise_id: String, timeout_ms: f64| -> String {
                guarded(|| match HostCalls.await_promise_ms(&promise_id, timeout_ms as i64) {
                    (_, _, Some(e)) => json!({ "err": e }),
                    (value, timed_out, None) => json!({ "ok": { "value": value, "timedOut": timed_out } }),
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_resolve_promise",
            Func::from(|promise_id: String, value: String| -> String {
                guarded(|| match HostCalls.resolve_promise(&promise_id, &value) {
                    Ok(()) => json!({ "ok": null }),
                    Err(e) => json!({ "err": e }),
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_reject_promise",
            Func::from(|promise_id: String, error: String| -> String {
                guarded(|| match HostCalls.reject_promise(&promise_id, &error) {
                    Ok(()) => json!({ "ok": null }),
                    Err(e) => json!({ "err": e }),
                })
            }),
        )
        .unwrap();
        g.set("__cleat_shims_off", SHIMS_OFF).unwrap();
        ctx.eval::<(), _>(include_str!("shims.js")).unwrap();
    });
    runtime
}

fn config() -> Config {
    let mut c = Config::default();
    // Needed so the promise continuations of an `async` workflow are drained after the entry function returns; without
    // it Javy refuses a pending job. A promise that never settles (a suspension) is not an error with it on.
    c.event_loop(true);
    c
}

#[unsafe(export_name = "initialize-runtime")]
pub extern "C" fn initialize_runtime() {
    javy_plugin_api::initialize_runtime(config, modify_runtime).unwrap();
}
