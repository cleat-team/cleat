//! The Rust fixture for the host-call execution harness: it runs exactly ONE
//! host call per invocation and reports what that call did.
//!
//! Mirrors `testdata/hostcallsgo/main.go` deliberately, argument for argument
//! — the same run IDs, the same promise name, the same service and plugin
//! names. Two languages that disagree about a call are only interesting if
//! they were asked the same question, and a fixture that quietly passed
//! different arguments would report a difference in the FIXTURES as a
//! difference in the SDKs.
//!
//! One call per invocation, for the reasons the Go fixture states: a host call
//! may suspend, and everything after it in the same invocation then never runs
//! — indistinguishable from having run and returned nothing.
//!
//! ## Suspension does not need special handling here, and that is not obvious
//!
//! In Go a suspending call panics with `cleat.ErrSuspend`. In Rust it returns
//! `CallError::Suspended` AND sets a thread-local flag, and `#[cleat_entry]`
//! consults that flag BEFORE it looks at the returned value (see the
//! `From<CallError> for String` doc comment in `cleat-sdk/src/lib.rs`). So a
//! branch below that formats a suspension into an ordinary `outcome` still
//! suspends the workflow, and the harness still sees a suspension rather than
//! that outcome. The formatting is unreachable in those cases, which is why it
//! is written to be harmless rather than to be clever.

use cleat_macro::cleat_entry;
// `HostCalls` is deliberately NOT imported. `#[cleat_entry]` rebuilds the
// inner function's first parameter as `h: &cleat_sdk::HostCalls`, fully
// qualified (`cleat-macro/src/entry.rs:92`), so a source-level import of the
// bare name is unused and warns. testdata/rustworkflow imports it only because
// it also names the type in a plain helper function, which this fixture does
// not.
use cleat_sdk::{CallError, ChildWorkflowOptions, RetryPolicy};
use serde::{Deserialize, Serialize};
use std::time::Duration;

/// What the harness sends: the name of the single call to exercise.
#[derive(Deserialize)]
struct Request {
    call: String,
}

/// What the harness reads back.
///
/// `status` is "ok", "error", or "unsupported". A call that suspends produces
/// no outcome at all — the harness detects that from the engine's suspend
/// result.
#[derive(Serialize)]
struct Outcome {
    call: String,
    status: String,
    detail: String,
}

/// Returns the `Outcome` itself, NOT a JSON string of it.
///
/// This is a real difference from the Go fixture and it is not cosmetic. Go's
/// `ExerciseHostCall` returns `(string, error)` and the generated adapter
/// passes that string through verbatim, so the fixture serialises the outcome
/// itself. `#[cleat_entry]` serialises whatever the function returns — so a
/// Rust fixture returning `Result<String, String>` produces a JSON *string
/// containing* JSON, and the harness reads
///
///     "{\"call\":\"DurableCall\",\"status\":\"ok\",...}"
///
/// which fails to decode into hostCallOutcome. Measured, not predicted: that
/// is exactly what the first version of this fixture did on all 20 of its
/// non-suspending calls.
///
/// Returning the typed value is also what a real Rust workflow does, so this
/// fixture now exercises the same serialisation path users get.
fn emit(call: &str, status: &str, detail: String) -> Result<Outcome, String> {
    Ok(Outcome {
        call: call.to_string(),
        status: status.to_string(),
        detail,
    })
}

fn ok(call: &str, detail: String) -> Result<Outcome, String> {
    emit(call, "ok", detail)
}

fn bad(call: &str, err: String) -> Result<Outcome, String> {
    emit(call, "error", err)
}

/// `unsupported` is a first-class outcome, not an error dressed up as one.
///
/// Two of the 24 wave-1 calls cannot be made from a Rust guest at all:
/// `crates/cleat-sdk/src/host_calls.rs` declares no `cleat_schedule_cron` and
/// no `cleat_list_crons` import, and contains the string "cron" zero times.
/// Recording that as `error` would put a missing binding in the same bucket as
/// a binding that ran and was refused by the host, which is precisely the
/// distinction this harness exists to draw.
///
/// It is also the outcome that reddens usefully: the day the Rust SDK gains
/// cron, this row starts reporting `ok` or `error`, stops matching its table
/// row, and someone has to decide what the right answer is.
fn unsupported(call: &str, detail: &str) -> Result<Outcome, String> {
    emit(call, "unsupported", detail.to_string())
}

/// Runs the one call named in the input.
///
/// Entry point: `exercise_host_call`
#[cleat_entry]
fn exercise_host_call(h: &HostCalls, input: Request) -> Result<Outcome, String> {
    let call = input.call.as_str();

    match call {
        // ---- children ----
        "ChildWorkflow" => match h.child_workflow("child-workflow", "{}") {
            (r, None) => ok(call, r),
            (_, Some(e)) => bad(call, e),
        },

        "ChildWorkflowWithOptions" => {
            let opts = ChildWorkflowOptions {
                version: 1,
                parent_close_policy: String::new(),
                priority: 0,
            };
            match h.child_workflow_with_options("child-workflow", "{}", &opts) {
                (r, None) => ok(call, r),
                (_, Some(e)) => bad(call, e),
            }
        }

        "AwaitChild" => match h.await_child("00000000-0000-0000-0000-000000000001") {
            Ok(r) => ok(call, r),
            Err(e) => bad(call, e.to_string()),
        },

        // Go's AwaitAllChildren returns a typed slice and the fixture reports
        // its length. Rust's returns the raw JSON array, so the count is taken
        // from the decoded array rather than from a Vec — same number, and the
        // detail is written to match Go's wording so the two tables can be
        // compared by eye.
        "AwaitAllChildren" => {
            match h.await_all_children(&["00000000-0000-0000-0000-000000000001"]) {
                Ok(r) => {
                    let n = serde_json::from_str::<serde_json::Value>(&r)
                        .ok()
                        .and_then(|v| v.as_array().map(|a| a.len()))
                        .unwrap_or(0);
                    ok(call, format!("{n} child result(s)"))
                }
                Err(e) => bad(call, e.to_string()),
            }
        }

        // NOTE a real SDK difference, recorded rather than smoothed over: Go's
        // AwaitAnyChild returns (runID, result) and Rust's returns one String.
        // The Go row asserts a detail of "runID result"; this one cannot, and
        // its table row says so instead of pretending the shapes match.
        "AwaitAnyChild" => match h.await_any_child(&["00000000-0000-0000-0000-000000000001"]) {
            Ok(r) => ok(call, r),
            Err(e) => bad(call, e.to_string()),
        },

        // Same shape difference: Go returns (status, result), Rust returns one
        // String.
        "PollChild" => match h.poll_child("00000000-0000-0000-0000-000000000001") {
            (r, None) => ok(call, r),
            (_, Some(e)) => bad(call, e),
        },

        // ---- promises ----
        "CreatePromise" => match h.create_promise("harness-promise") {
            (id, None) => ok(call, id),
            (_, Some(e)) => bad(call, e),
        },

        "AwaitPromise" => {
            match h.await_promise(
                "00000000-0000-0000-0000-000000000002",
                Duration::from_millis(10),
            ) {
                (r, timed_out, None) => ok(call, format!("timedOut={timed_out} {r}")),
                (_, _, Some(e)) => bad(call, e),
            }
        }

        // ---- signals ----
        "DurableAwaitSignals" => {
            match h.await_signals(&["harness-signal"], Duration::from_millis(10)) {
                Ok(s) => ok(
                    call,
                    format!("timedOut={} {} {}", s.timed_out, s.name, s.payload),
                ),
                Err(e) => bad(call, e.to_string()),
            }
        }

        "PollSignal" => match h.poll_signal("harness-signal") {
            (payload, present, None) => ok(call, format!("present={present} {payload}")),
            (_, _, Some(e)) => bad(call, e),
        },

        // ---- durable calls ----
        "DurableCall" => match h.call("harness-service", "harness-op", "{}") {
            (r, None) => ok(call, r),
            (_, Some(e)) => bad(call, e),
        },

        "DurableCallWithHeartbeat" => {
            // Go passes a progress callback; the Rust binding takes none, so
            // the two rows prove different amounts. Recorded in the table.
            match h.cleat_call_heartbeat("harness-service", "harness-op", "{}", 1000) {
                (r, None) => ok(call, r),
                (_, Some(e)) => bad(call, e),
            }
        }

        "DurableCallWithRetry" => {
            let policy = RetryPolicy {
                max_attempts: 1,
                initial_interval_ms: 1,
                backoff_multiplier: 1.0,
                maximum_interval_ms: 1,
                non_retryable_errors: vec![],
            };
            // Typed on both ends: the Rust binding is generic and there is no
            // string-in/string-out form. serde_json::Value is the closest
            // thing to Go's raw string, and the round trip through it is part
            // of what this row exercises.
            let req = serde_json::json!({});
            match h.cleat_call_with_retry::<serde_json::Value, serde_json::Value>(
                "harness-service",
                "harness-op",
                &req,
                &policy,
            ) {
                Ok(v) => ok(call, v.to_string()),
                Err(e) => bad(call, e.to_string()),
            }
        }

        // ---- defers ----
        "DurableDefer" => match h.cleat_defer("harness defer") {
            (id, None) => ok(call, id),
            (_, Some(e)) => bad(call, e),
        },

        "DurableDeferFunc" => match h.defer_func(|| Ok(())) {
            (id, None) => ok(call, id),
            (_, Some(e)) => bad(call, e),
        },

        // ---- crons: not reachable from this SDK ----
        //
        // Re-derive before changing either arm:
        //
        //     grep -c cron crates/cleat-sdk/src/host_calls.rs        # 0
        //     grep -c cleat_schedule_cron packages/cleat-as/assembly/host-calls.ts   # non-zero
        //
        // AssemblyScript declares both imports; Rust declares neither. The
        // host exports them (`engine/imports.go`), so this is a guest-side
        // gap, which is the category this whole round is about.
        "ScheduleCron" => unsupported(
            call,
            "cleat-sdk declares no cleat_schedule_cron import; the host exports it and the AssemblyScript SDK binds it",
        ),

        "ListCrons" => unsupported(
            call,
            "cleat-sdk declares no cleat_list_crons import; the host exports it and the AssemblyScript SDK binds it",
        ),

        // ---- plugins ----
        //
        // Both paths in one row, for the reason the Go fixture gives at
        // length: llm.list_models is the only plugin call that works with no
        // tenant, so on its own it exercises the success path only, and
        // §3.200 lived entirely on the error path.
        "PluginCall" => {
            let (r, err) = h.plugin_call("llm", "list_models", "{}");
            if let Some(e) = err {
                return bad(call, e);
            }
            match h.plugin_call("blobstore", "put", r#"{"key":"k","data":"aGk="}"#) {
                (_, Some(e)) => ok(call, format!("ok={r} err={e}")),
                (_, None) => ok(
                    call,
                    format!("ok={r} err=<none: blobstore.put unexpectedly succeeded>"),
                ),
            }
        }

        // Go's binding returns a channel and the fixture counts events. Rust's
        // returns the buffered response, so this row reports the response and
        // NOT an event count — a real difference in what the two SDKs offer
        // for the same host call, and one the table records rather than hides.
        "PluginCallStreaming" => match h.plugin_call_streaming(
            "llm",
            "chat_stream",
            r#"{"messages":[{"role":"user","content":"hi"}]}"#,
        ) {
            (r, None) => ok(call, r),
            (_, Some(e)) => bad(call, e),
        },

        // ---- locks ----
        //
        // Twice, for the same reason the Go fixture does it twice: a row that
        // only ever sees `true` cannot tell a decoded bit from a hardcoded
        // one. It does not close that gap here either — the in-memory lock is
        // re-entrant for the same holder — and the table row says so.
        "AcquireLock" => {
            let (first, err) = h.acquire_lock("harness-lock", Duration::from_secs(60));
            if let Some(e) = err {
                return bad(call, e);
            }
            let (second, err) = h.acquire_lock("harness-lock", Duration::from_secs(60));
            if let Some(e) = err {
                return bad(call, e);
            }
            ok(call, format!("first={first} second={second}"))
        }

        // ---- identity and control ----
        "WorkflowID" => ok(call, h.workflow_id()),

        "RunID" => ok(call, h.run_id()),

        "PollCancellation" => {
            let (cancelled, reason) = h.poll_cancellation();
            ok(call, format!("cancelled={cancelled} {reason}"))
        }

        // NOTE the largest SDK difference in this fixture: Go's SideEffect
        // takes a closure and the host caches its result; Rust's takes the
        // already-computed value. So the Go row proves the closure ran and its
        // value round-tripped, and this row proves only the round trip. The
        // value is the same string so the two details match, which makes the
        // difference easy to miss — hence this comment and the table row.
        "SideEffect" => match h.side_effect("side-effect-value") {
            Ok(r) => ok(call, r),
            Err(e) => bad(call, e),
        },

        // An unknown name is a hard error, not an "error" outcome: the harness
        // drives this fixture from the same list it asserts against, so a name
        // it does not recognise means the two have drifted.
        other => Err(format!("fixture: no case for host call {other:?}")),
    }
}

// Keeps `CallError` referenced even if every arm above stops using it, so the
// import cannot rot into a warning that the build denies.
#[allow(dead_code)]
fn _assert_call_error_displays(e: CallError) -> String {
    e.to_string()
}
