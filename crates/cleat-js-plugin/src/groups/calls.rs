//! The `calls` group of host-call bindings (cleat#3827): the durable-call variants beyond `cleat_call`.
//!
//! FOUR CALLS, TWO SHAPES. `cleat_send` and `cleat_schedule_invoke` are fire-and-forget and answer a status. `call_heartbeat` and
//! `call_retry` answer a response or an error. All four write, so all four go through `guarded`.
//!
//! THE RETRY CALL USES THE HOST-ONLY ENTRY POINT, and a policy the host refuses is REPORTED rather than retried here.
//!
//! The SDKs fall back to a loop of their own when the host declines a policy as too long for one segment, and this binding
//! deliberately does not: that fallback is broken on replay, for every guest language, because the host's retry entry point
//! answers a replay with the recorded first attempt (`engine/durablecalls.go`, `replayRetryAttempts` matches only
//! `call_attempt_failed` records, so an ordinary `call` event falls through to `replayCall`, which returns that attempt's
//! error). Measured here, not inferred: the fallback path failed its second segment with the first attempt's message. The engine
//! fix is to answer a refused policy before consulting history; until then a guest that cannot fall back safely must say so,
//! which is what the refusal below does.
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

/// The message a refused policy carries. Named here rather than inline so the test and the doc quote the same sentence.
const POLICY_TOO_LONG: &str = "cleat: this retry policy's worst-case backoff exceeds the host-retry budget, so the host declined to run it in one segment. TypeScript does not fall back to an SDK-level loop (see cleat#4032); use a shorter policy.";

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
                    match HostCalls.cleat_call_with_host_retry::<Value, Value>(
                        &service, &operation, &req, &policy,
                    ) {
                        // JSON text, as `__cleat_call` answers, so the TS layer has one decoding rule for every call.
                        Ok(v) => json!({ "ok": v.to_string() }),
                        // A suspension is reported by the flag `guarded` reads on the way out, so the envelope is replaced before
                        // it can reach the guest; this arm exists only so the match is total.
                        Err(CallError::Suspended) => json!({ "err": "suspended" }),
                        // Nothing was called and no attempt was consumed: the host declined the policy itself. Named as its own
                        // refusal rather than as a call failure, because the caller's fix is a smaller policy and not a service
                        // that failed.
                        Err(CallError::RetryPolicyTooLong) => json!({ "err": POLICY_TOO_LONG }),
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
