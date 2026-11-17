//! The `calls` group of host-call bindings (cleat#3827): the durable-call variants beyond `cleat_call`.
//!
//! FOUR CALLS, TWO SHAPES. `cleat_send` and `cleat_schedule_invoke` are fire-and-forget and answer a status. `call_heartbeat` and
//! `call_retry` answer a response or an error. All four write, so all four go through `guarded`.
//!
//! THE RETRY CALL FALLS BACK LIKE THE OTHER SDKS DO, because cleat#4032 fixed the thing that made that unsafe.
//!
//! When the host declines a policy as too long for one segment, `cleat_call_with_retry` runs the policy itself: one attempt per
//! segment, sleeping between them. That fallback used to be broken on replay for EVERY guest language -- the host's retry entry
//! answered a replay with the recorded first attempt, because `replayRetryAttempts` matches only `call_attempt_failed` records
//! and an ordinary `call` event fell through to `replayCall`. #4032 now records the refusal and answers a replay with it, so the
//! guest's loop replays its own attempts. This binding uses the same entry point Go's `DurableCallWithOptions` and Rust's
//! `cleat_call_with_retry` use, and that is what the PR that removed the workaround changed.
//!
//! Conventions: `packages/cleat-ts/groups/README.md`.

use cleat_sdk::{CallError, HostCalls, RetryPolicy};
use javy_plugin_api::javy::quickjs::Object;
use javy_plugin_api::javy::quickjs::prelude::Func;
use serde_json::{Value, json};

use crate::{guarded, pair};

/// The policy as the TS layer writes it: camelCase, and the names the majority of the SDKs use.
///
/// Read field by field rather than through `#[derive(Deserialize)]`, because `serde` is not a direct dependency of this crate --
/// only `serde_json` is -- and adding one would change `Cargo.lock`, a file nine groups are written against in parallel. Note
/// that `cleat-sdk`'s own `RetryPolicy` has NO serde rename, so handing it the guest's JSON directly fails on `missing field
/// max_attempts`; that was measured, not guessed. The field that differs most between SDKs is the multiplier: Rust calls it
/// `backoff_multiplier`, while Go, Python and the AssemblyScript docs call it a coefficient.
struct PolicyJSON {
    max_attempts: u32,
    initial_interval_ms: u64,
    backoff_coefficient: f64,
    max_interval_ms: u64,
    non_retryable_errors: Vec<String>,
}

fn parse_policy(text: &str) -> Result<PolicyJSON, String> {
    let v: Value =
        serde_json::from_str(text).map_err(|e| format!("the retry policy was not JSON: {e}"))?;
    let int = |name: &str| -> Result<i64, String> {
        v.get(name)
            .and_then(Value::as_i64)
            .ok_or_else(|| format!("the retry policy has no integer \"{name}\" field"))
    };
    let max_attempts = int("maxAttempts")?;
    let initial = int("initialIntervalMs")?;
    let max_interval = int("maxIntervalMs")?;
    let coefficient = v
        .get("backoffCoefficient")
        .and_then(Value::as_f64)
        .ok_or_else(|| {
            "the retry policy has no numeric \"backoffCoefficient\" field".to_string()
        })?;
    // Checked here rather than only in the TS layer, because a negative interval would otherwise reach the host as a wrapped
    // unsigned value and be read as an enormous one.
    if max_attempts < 1 || initial < 0 || max_interval < 0 {
        return Err(format!(
            "the retry policy is out of range: maxAttempts {max_attempts}, initialIntervalMs {initial}, maxIntervalMs {max_interval}"
        ));
    }
    let non_retryable: Vec<String> = match v.get("nonRetryableErrors") {
        None | Some(Value::Null) => Vec::new(),
        Some(list) => serde_json::from_value(list.clone()).map_err(|e| {
            format!("the retry policy's nonRetryableErrors was not an array of strings: {e}")
        })?,
    };
    Ok(PolicyJSON {
        max_attempts: max_attempts as u32,
        initial_interval_ms: initial as u64,
        backoff_coefficient: coefficient,
        max_interval_ms: max_interval as u64,
        non_retryable_errors: non_retryable,
    })
}

pub(crate) fn install(g: &Object<'_>) {
    // (service, operation, request, policyJSON). The policy crosses as one JSON OBJECT rather than five positional numbers
    // because rquickjs binds at most seven arguments to a closure, and because a JSON object is the one form whose field
    // names are readable at the boundary -- `backoffCoefficient: 2` rather than a bare `200` scaled to hundredths.
    g.set(
        "__cleat_call_retry",
        Func::from(
            |service: String, operation: String, request: String, policy_json: String| -> String {
                guarded(|| {
                    let raw = match parse_policy(&policy_json) {
                        Ok(p) => p,
                        Err(e) => return json!({ "err": e }),
                    };
                    let policy = RetryPolicy {
                        max_attempts: raw.max_attempts as u32,
                        initial_interval_ms: raw.initial_interval_ms as u64,
                        backoff_multiplier: raw.backoff_coefficient,
                        maximum_interval_ms: raw.max_interval_ms as u64,
                        non_retryable_errors: raw.non_retryable_errors,
                    };
                    // The SDK's typed entry point wants a Serialize value; the guest already handed us JSON text, so parse it
                    // rather than asking the TS layer for an object. A request that is not JSON is refused here instead of being
                    // sent as the JSON string of a string.
                    let req: Value = match serde_json::from_str(&request) {
                        Ok(v) => v,
                        Err(e) => return json!({ "err": format!("the request was not JSON: {e}") }),
                    };
                    // `cleat_call_with_retry`, NOT the host-only entry: it asks the host first and, when the host declines
                    // the policy as too long for one segment, runs it itself -- one attempt per segment, sleeping between
                    // them. That fallback was broken until cleat#4032 recorded the refusal; on a replay the host now refuses
                    // the same policy again instead of answering from history, so the SDK's loop replays its own recorded
                    // attempts. This is the same shape Go's `DurableCallWithOptions` and Rust's `cleat_call_with_retry` have.
                    match HostCalls.cleat_call_with_retry::<Value, Value>(
                        &service, &operation, &req, &policy,
                    ) {
                        // JSON text, as `__cleat_call` answers, so the TS layer has one decoding rule for every call.
                        Ok(v) => json!({ "ok": v.to_string() }),
                        // A suspension is reported by the flag `guarded` reads on the way out, so the envelope is replaced before
                        // it can reach the guest; this arm exists only so the match is total.
                        Err(CallError::Suspended) => json!({ "err": "suspended" }),
                        // Unreachable through `cleat_call_with_retry`, which handles this by falling back. Kept so the match
                        // stays total, and worded as the refusal it is rather than as a service failure.
                        Err(CallError::RetryPolicyTooLong) => json!({ "err": "cleat: the host declined this retry policy as too long for one segment, and the fallback did not run it" }),
                        Err(CallError::Failed(msg)) => json!({ "err": msg }),
                        Err(e) => json!({ "err": format!("{e}") }),
                    }
                })
            },
        ),
    )
    .unwrap();
    g.set(
        "__cleat_call_heartbeat",
        Func::from(
            |service: String,
             operation: String,
             request: String,
             heartbeat_interval_ms: f64|
             -> String {
                guarded(|| {
                    pair(HostCalls.cleat_call_heartbeat(
                        &service,
                        &operation,
                        &request,
                        heartbeat_interval_ms as i64,
                    ))
                })
            },
        ),
    )
    .unwrap();
    g.set(
        "__cleat_send",
        Func::from(
            |service: String, operation: String, request: String| -> String {
                guarded(
                    || match HostCalls.cleat_send(&service, &operation, &request) {
                        Ok(()) => json!({ "ok": null }),
                        Err(e) => json!({ "err": e }),
                    },
                )
            },
        ),
    )
    .unwrap();
    g.set(
        "__cleat_schedule_invoke",
        Func::from(
            |service: String, operation: String, request: String, delay_ms: f64| -> String {
                guarded(|| {
                    match HostCalls.schedule_invoke_ms(
                        &service,
                        &operation,
                        &request,
                        delay_ms as i64,
                    ) {
                        Ok(()) => json!({ "ok": null }),
                        Err(e) => json!({ "err": e }),
                    }
                })
            },
        ),
    )
    .unwrap();
}
