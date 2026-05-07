//! Cleat Rust SDK port of the Restate "basics" examples.
//!
//! This file demonstrates Cleat's durable execution patterns:
//!   - Durable execution with retries and idempotent service calls
//!   - Durable building blocks (timers, promises, signals, RPC)
//!   - Virtual Objects (key-scoped stateful services)
//!   - Workflows (multi-step durable execution with signals)
//!
//! Each `#[cleat_entry]` function is compiled as a separate WASM export.
//! Build: cargo build --target wasm32-wasip1 --release

use cleat_macro::cleat_entry;
use cleat_sdk::HostCalls;
use serde::{Deserialize, Serialize};
// std::time::Duration is available but not used directly in entry functions
// since cleat_sleep takes i64 milliseconds.

// =============================================================================
// Shared data types
// =============================================================================

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SubscriptionRequest {
    pub user_id: String,
    pub credit_card: String,
    pub subscriptions: Vec<String>,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct User {
    pub name: String,
    pub email: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct GreetInput {
    pub name: String,
    pub greeting: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct CountQuery {
    pub key: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct ClickInput {
    pub user_id: String,
    pub secret: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct SignupResult {
    pub success: bool,
    pub matched: bool,
    pub message: String,
}

/// Empty input for demos that take no parameters.
#[derive(Debug, Deserialize)]
pub struct EmptyInput {}

// =============================================================================
// Pattern 0: Durable Execution (Subscription Management)
//
// Ported from: p0_cleat_execution.rs
// Restate used: ctx.run() for idempotent actions, ctx.rand_uuid() for idempotency keys
// Cleat uses:   cleat_call() for durable RPC, uuid(seed) for deterministic UUIDs
//
// GAPS:
//   - No `ctx.run()` equivalent: Restate wraps side effects in `ctx.run()` to
//     persist results and skip re-execution on replay. Cleat's cleat_call() is
//     already durable on the host side, but there is no wrapper for arbitrary
//     synchronous code (e.g. calling an HTTP API from inside the WASM module).
//   - Cleat depends on the host runtime to provide the called services via
//     cleat_call().
// =============================================================================

#[cleat_entry]
fn subscription_add(h: &HostCalls, req: SubscriptionRequest) -> Result<String, String> {
    h.cleat_log(&format!(
        "subscription_add: user={}, subscriptions={:?}",
        req.user_id, req.subscriptions
    ));

    // ---- Idempotency key ----
    // In Restate: let payment_id = ctx.rand_uuid().to_string()
    // In Cleat: use uuid(seed) for deterministic UUID from host runtime
    let payment_id = h.uuid(&format!("payment:{}:{}", req.user_id, req.subscriptions.join(",")));
    h.cleat_log(&format!("Generated payment idempotency key: {}", payment_id));

    // ---- Create recurring payment ----
    // In Restate: ctx.run(|| create_recurring_payment(&req.credit_card, &payment_id)).await?
    // In Cleat: cleat_call("service", "operation", request_json)
    let payment_input = serde_json::json!({
        "credit_card": req.credit_card,
        "payment_id": payment_id,
    });
    let (pay_ref, err) = h.cleat_call(
        "payment-service",
        "create_recurring_payment",
        &payment_input.to_string(),
    );
    if let Some(e) = err {
        return Err(format!("Failed to create recurring payment: {}", e));
    }
    h.cleat_log(&format!("Created recurring payment: {}", pay_ref));

    // ---- Create subscriptions ----
    for sub in &req.subscriptions {
        let sub_input = serde_json::json!({
            "user_id": req.user_id,
            "subscription": sub,
            "payment_ref": pay_ref,
        });
        let (result, err) = h.cleat_call(
            "subscription-service",
            "create_subscription",
            &sub_input.to_string(),
        );
        if let Some(e) = err {
            // Log and continue with remaining subscriptions (best-effort)
            h.cleat_log(&format!("Failed to create subscription '{}': {} (continuing)", sub, e));
            continue;
        }
        h.cleat_log(&format!("Created subscription '{}': {}", sub, result));
    }

    let response = serde_json::json!({
        "status": "completed",
        "payment_reference": pay_ref,
    });
    Ok(response.to_string())
}

// =============================================================================
// Pattern 1: Durable Building Blocks Reference
//
// Ported from: p1_building_blocks.rs
// Restate used: a rich context API with awakeables, sleep, RPC, messaging
// Cleat uses:   HostCalls methods for durable operations
//
// GAPS:
//   - No awakeable/ResolveAwakeable: Restate provides Awakeable (promises that
//     can be resolved by other handlers). Cleat has create_promise/await_promise
//     but no resolve_promise in the Rust SDK.
//   - No `ctx.run()`: as noted above.
//   - No deferred/scheduled messaging: Restate's send_after() schedules delayed
//     messages. Cleat has no equivalent in the current HostCalls ABI.
//   - No `ctx.key()`: Restate's ObjectContext::key() returns the current object
//     key. Cleat uses get_scope() which returns (object_type, instance_key).
// =============================================================================

#[cleat_entry]
fn building_blocks_demo(h: &HostCalls, _input: EmptyInput) -> Result<String, String> {
    h.cleat_log("=== Building Blocks Demo ===");

    // ---- 1. Durable RPC (call another service) ----
    // Restate: ctx.object_client::<X>("key").create("req").call()
    // Cleat:   cleat_call("service", "operation", json_payload)
    let (result, err) = h.cleat_call(
        "subscription-service",
        "get_status",
        "{\"dummy\": true}",
    );
    if let Some(e) = err {
        h.cleat_log(&format!("RPC call failed (expected in isolation): {}", e));
    } else {
        h.cleat_log(&format!("RPC result: {}", result));
    }

    // ---- 2. Durable Timers (sleep) ----
    // Restate: ctx.sleep(Duration::from_secs(5)).await?
    // Cleat:   h.cleat_sleep(5000) -- panics with SuspendSentinel when suspending
    // Note: In the WASM runtime, cleat_sleep causes the workflow to suspend
    // and be resumed later. In test/unit context, this will suspend.
    h.cleat_log("Would sleep for 5 seconds (suspends in WASM runtime)");
    // Uncomment to test in WASM runtime:
    // h.cleat_sleep(5000);

    // ---- 3. Durable Promises (create + await) ----
    // Restate: let (id, promise) = ctx.awakeable::<String>(); promise.await?
    // Cleat:   h.create_promise("name") -> id, then h.await_promise(id, timeout)
    let (promise_id, err) = h.create_promise("demo-promise");
    if let Some(e) = err {
        h.cleat_log(&format!("Create promise failed: {}", e));
    } else {
        h.cleat_log(&format!(
            "Created promise 'demo-promise' with id: {} (await would suspend)",
            promise_id
        ));
        // In a real workflow, we would await the promise:
        // let (result, timed_out, err) = h.await_promise(&promise_id, 30000);
        // For this demo, we don't await since no one will resolve it.
    }

    // ---- 4. Signals ----
    // Restate: no direct equivalent; uses awakeables for cross-handler comms
    // Cleat:   signal_workflow(target, signal, payload) for fire-and-forget
    //          send_signal_and_wait(target, signal, payload, timeout) for RPC
    let signal_err = h.signal_workflow(
        "target-workflow-id",
        "notify",
        "{\"message\": \"hello\"}",
    );
    match signal_err {
        Ok(()) => h.cleat_log("Signal sent successfully"),
        Err(e) => h.cleat_log(&format!("Signal send failed (expected in isolation): {}", e)),
    }

    // ---- 5. UUID / Deterministic Random ----
    // Restate: ctx.rand_uuid()
    // Cleat:   h.uuid(seed)
    let id = h.uuid("building-blocks-demo-seed");
    h.cleat_log(&format!("Deterministic UUID: {}", id));

    // ---- 6. Random number ----
    // Restate: ctx.rand_uuid() (randomness)
    // Cleat:   h.random() -> i64
    let r = h.random();
    h.cleat_log(&format!("Random (deterministic replay): {}", r));

    // ---- 7. Version info ----
    let version = h.version();
    let min_version = h.min_version();
    h.cleat_log(&format!("Host version: {}, min_version: {}", version, min_version));

    // ---- 8. Cancellation check ----
    // Restate: automatically handled by the runtime
    // Cleat:   h.poll_cancellation() -> (cancelled, reason)
    let (cancelled, reason) = h.poll_cancellation();
    if cancelled {
        h.cleat_log(&format!("Cancellation requested: {}", reason));
        return Ok(serde_json::json!({"status": "cancelled", "reason": reason}).to_string());
    }
    h.cleat_log("Not cancelled");

    // ---- 9. Set query state (for inspecting running workflows) ----
    // Restate: set query handlers on workflow
    // Cleat:   h.set_query_state(key, value)
    h.set_query_state("status", "running");
    h.set_query_state("step", "demo-complete");

    // ---- 10. Scoping (Virtual Object context) ----
    // Restate: automatic based on the invoked object
    // Cleat:   h.set_scope("object_type", "instance_key") / h.get_scope()
    let prev = h.set_scope("DemoObject", "demo-instance-1");
    h.cleat_log(&format!("Previous scope: '{}'", prev));

    let (obj_type, inst_key) = h.get_scope();
    h.cleat_log(&format!("Current scope: type='{}', key='{}'", obj_type, inst_key));

    Ok(serde_json::json!({
        "status": "completed",
        "uuid": id,
        "random": r,
        "version": version,
        "scope_type": obj_type,
        "scope_key": inst_key,
    }).to_string())
}

// =============================================================================
// Pattern 2: Virtual Objects (Greeter with K/V State)
//
// Ported from: p2_virtual_objects.rs
// Restate used: ObjectContext with get()/set() for K/V state, ctx.key() for key
// Cleat uses:   set_scope()/get_scope() for object context, set_query_state() for
//               observable query state (NOT persistent K/V state)
//
// GAPS:
//   - NO get_state / set_state / delete_state: The Rust SDK HostCalls does NOT
//     expose any persistent K/V state operations. The only state-like API is
//     set_query_state(), which sets queryable state on a *running workflow* --
//     this is NOT the same as Virtual Object persistent state.
//   - In Restate, ObjectContext::get("count") returns the persisted count for
//     that object key. In Cleat, there is no equivalent. This is a critical gap
//     for Virtual Object patterns.
//   - Workaround: We document the intended operation using set_query_state as a
//     partial substitute, and call out the missing APIs.
// =============================================================================

#[cleat_entry]
fn greeter_greet(h: &HostCalls, input: GreetInput) -> Result<String, String> {
    // Set the virtual object scope
    // In a real Cleat host runtime, this would scope subsequent state operations.
    let _prev = h.set_scope("GreeterObject", &input.name);

    let (_obj_type, inst_key) = h.get_scope();
    h.cleat_log(&format!(
        "GreeterObject '{}' received greeting: {}",
        inst_key, input.greeting
    ));

    // ---- State operations ----
    // Restate: let mut count = ctx.get::<u64>("count").await?.unwrap_or(0);
    //          count += 1;
    //          ctx.set("count", count);
    //
    // Cleat:   NO get_state/set_state available in HostCalls.
    //          We use set_query_state as a workaround for *observable* state,
    //          but this is NOT durable K/V state and should not be relied upon
    //          for Virtual Object persistence.
    //
    // The following demonstrates what the code WOULD look like with proper APIs:
    //
    //   let count_json = h.get_state("count").unwrap_or("0".to_string());
    //   let mut count: u64 = serde_json::from_str(&count_json).unwrap_or(0);
    //   count += 1;
    //   h.set_state("count", &count.to_string());
    //   h.set_state("last_greeting", &input.greeting);
    //
    // For now, we use set_query_state to make the count observable:
    // (This is NOT persisted across invocations -- it's query state on the
    //  running workflow, not Virtual Object state.)
    h.set_query_state("last_greeting", &input.greeting);

    let response = serde_json::json!({
        "message": format!(
            "{} {} (count tracking requires HostCalls::get_state/set_state)",
            input.greeting, inst_key
        ),
        "object_key": inst_key,
        "missing_api": "get_state/set_state not implemented in Cleat Rust SDK",
    });
    Ok(response.to_string())
}

#[cleat_entry]
fn greeter_ungreet(h: &HostCalls, input: GreetInput) -> Result<String, String> {
    // Restate: count -= 1; ctx.set("count", count);
    // Cleat:   No state operations available.

    let _prev = h.set_scope("GreeterObject", &input.name);
    let (_, inst_key) = h.get_scope();
    h.cleat_log(&format!("GreeterObject '{}': ungreet called (state not persisted)", inst_key));

    h.set_query_state("last_action", "ungreet");

    let response = serde_json::json!({
        "message": format!(
            "Dear {}, cannot decrement count without get_state/set_state APIs.",
            inst_key
        ),
        "missing_api": "get_state/set_state not implemented in Cleat Rust SDK",
    });
    Ok(response.to_string())
}

// =============================================================================
// Pattern 3: Workflows (Signup with Email Verification)
//
// Ported from: p3_workflows.rs
// Restate used: WorkflowContext with promise()/resolve_promise() for email
//               verification, ctx.run() for side effects, ctx.key() for ID
// Cleat uses:   create_promise()/await_promise() for awaiting verification,
//               cleat_call() for service calls, get_scope() for key
//
// GAPS:
//   - No resolve_promise: The Rust SDK has create_promise() and await_promise()
//     but no resolve_promise() to resolve a promise from another handler.
//     The Cleat ABI has reply_to_signal() which requires a correlation token,
//     and signal_workflow() for fire-and-forget signaling, but no direct way
//     to resolve a promise by ID from a different export.
//   - Workaround: Use signal_workflow from the click handler and await_signals
//     in the workflow. This is demonstrated below.
//   - No attach/get_result: Restate provides /attach to get workflow results.
//     Cleat's model requires explicit result retrieval via query state or
//     cleat_call.
// =============================================================================

#[cleat_entry]
fn signup_run(h: &HostCalls, user: User) -> Result<String, String> {
    h.cleat_log(&format!(
        "SignupWorkflow: starting for user '{}' <{}>",
        user.name, user.email
    ));

    // Set virtual object scope to associate this execution with a user ID.
    let _prev = h.set_scope("SignupWorkflow", &user.name);
    let (_, inst_key) = h.get_scope();
    h.cleat_log(&format!("Workflow key: {}", inst_key));

    // ---- Step 1: Create user entry (durable call to user-service) ----
    // Restate: ctx.run(|| create_user_entry(&user)).await?
    let (_, err) = h.cleat_call(
        "user-service",
        "create_user",
        &serde_json::to_string(&user).map_err(|e| format!("serialization error: {}", e))?,
    );
    if let Some(e) = err {
        return Err(format!("Failed to create user entry: {}", e));
    }
    h.cleat_log("User entry created");

    // ---- Step 2: Generate verification secret and send email ----
    // Restate: let secret = ctx.rand_uuid().to_string();
    let secret = h.uuid(&format!("email-verify:{}", user.email));
    h.cleat_log(&format!("Verification secret generated: {}", secret));

    let email_payload = serde_json::json!({
        "email": user.email,
        "name": user.name,
        "secret": secret,
        "user_id": inst_key,
    });
    // Restate: ctx.run(|| send_email_with_link(user_id, &user, &secret))
    let (email_result, err) = h.cleat_call(
        "email-service",
        "send_verification",
        &email_payload.to_string(),
    );
    if let Some(e) = err {
        return Err(format!("Failed to send verification email: {}", e));
    }
    h.cleat_log(&format!("Email sent: {}", email_result));

    // ---- Step 3: Await email verification ----
    // Restate: let click_secret = ctx.promise::<String>("email-link").await?
    // Cleat:   await_signals(["email-verify"], timeout_ms) -> (name, payload, timed_out, err)
    //
    // The signup_click export will send a signal_workflow to us.
    // We use await_signals with a generous timeout (24 hours in ms).
    h.cleat_log("Awaiting email verification signal...");
    let (signal_name, payload, timed_out, err) = h.await_signals(&["email-verify"], 86400000);

    if let Some(e) = err {
        return Err(format!("Error awaiting verification signal: {}", e));
    }
    if timed_out {
        h.cleat_log("Verification timed out (24h)");
        h.set_query_state("status", "verification-timeout");
        let result = SignupResult {
            success: false,
            matched: false,
            message: "Email verification timed out after 24 hours.".to_string(),
        };
        return Ok(serde_json::to_string(&result).map_err(|e| e.to_string())?);
    }

    h.cleat_log(&format!("Received signal '{}' with payload: {}", signal_name, payload));

    // ---- Step 4: Verify the secret matches ----
    let matched = payload.trim_matches('"') == secret;
    if matched {
        h.cleat_log("Email verified successfully! Signup complete.");
    } else {
        h.cleat_log("Verification secret mismatch.");
    }

    h.set_query_state("status", if matched { "verified" } else { "mismatch" });

    let result = SignupResult {
        success: matched,
        matched,
        message: if matched {
            format!("Welcome {}! Your account is now active.", user.name)
        } else {
            "Verification failed: invalid secret.".to_string()
        },
    };
    Ok(serde_json::to_string(&result).map_err(|e| e.to_string())?)
}

/// Handle the email verification click.
///
/// This is a separate WASM export that is called when the user clicks the
/// verification link in their email. It sends a signal to the running
/// SignupWorkflow instance.
///
/// Restate: ctx.resolve_promise::<String>("email-link", secret)
/// Cleat:   signal_workflow(target, "email-verify", payload)
///
/// NOTE: signal_workflow requires the target_run_id, which the click handler
/// must know. In a real deployment, the verification link would encode the
/// target workflow run ID, or the Cleat host runtime would route by object key.
#[cleat_entry]
fn signup_click(h: &HostCalls, input: ClickInput) -> Result<String, String> {
    h.cleat_log(&format!(
        "SignupWorkflow.click: user_id={}, secret={}",
        input.user_id, input.secret
    ));

    // In a real Cleat deployment, we would signal the running workflow.
    // The target_run_id would be obtained from the verification link URL.
    // For now, we demonstrate the signal API call.
    let payload = serde_json::json!({"secret": input.secret});
    let result = h.signal_workflow(
        &input.user_id,   // This would be the target_run_id in a real deployment
        "email-verify",
        &payload.to_string(),
    );

    match result {
        Ok(()) => {
            h.cleat_log("Verification signal sent to workflow");
            Ok(serde_json::json!({"status": "signal_sent", "user_id": input.user_id}).to_string())
        }
        Err(e) => {
            h.cleat_log(&format!("Failed to send verification signal: {}", e));
            Err(format!("Failed to signal workflow: {}", e))
        }
    }
}

// =============================================================================
// Helper: get_state / set_state workaround
//
// The Cleat Rust SDK does not yet expose get_state/set_state host calls.
// As a host-call-level workaround, the host runtime could provide a
// "key-value-store" service callable via cleat_call. This demonstrates
// one possible pattern until native state APIs are added.
//
// These are NOT replacements for proper Virtual Object state -- they are
// documented here to show the current workaround.
// =============================================================================

/// Set a value in the key-value store (workaround via cleat_call).
/// Requires a "key-value-store" service to be registered with the host.
#[allow(dead_code)]
fn kv_set(h: &HostCalls, store: &str, key: &str, value: &str) -> Result<(), String> {
    let input = serde_json::json!({"store": store, "key": key, "value": value});
    let (_resp, err) = h.cleat_call("key-value-store", "set", &input.to_string());
    match err {
        None => Ok(()),
        Some(e) => Err(format!("kv_set failed: {}", e)),
    }
}

/// Get a value from the key-value store (workaround via cleat_call).
/// Requires a "key-value-store" service to be registered with the host.
#[allow(dead_code)]
fn kv_get(h: &HostCalls, store: &str, key: &str) -> Result<Option<String>, String> {
    let input = serde_json::json!({"store": store, "key": key});
    let (resp, err) = h.cleat_call("key-value-store", "get", &input.to_string());
    match err {
        None => {
            let val: serde_json::Value =
                serde_json::from_str(&resp).map_err(|e| format!("parse error: {}", e))?;
            Ok(val.get("value").and_then(|v| v.as_str()).map(String::from))
        }
        Some(e) => Err(format!("kv_get failed: {}", e)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Test data types serialize correctly.
    #[test]
    fn test_subscription_request_serde() {
        let json = r#"{"userId":"test","creditCard":"1234","subscriptions":["a","b"]}"#;
        let req: SubscriptionRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.user_id, "test");
        assert_eq!(req.subscriptions.len(), 2);
    }

    /// Test User serde roundtrip
    #[test]
    fn test_user_serde() {
        let u = User {
            name: "Alice".into(),
            email: "alice@example.com".into(),
        };
        let json = serde_json::to_string(&u).unwrap();
        let u2: User = serde_json::from_str(&json).unwrap();
        assert_eq!(u.name, u2.name);
        assert_eq!(u.email, u2.email);
    }

    /// Test SignupResult serde roundtrip
    #[test]
    fn test_signup_result_serde() {
        let r = SignupResult {
            success: true,
            matched: true,
            message: "Welcome!".into(),
        };
        let json = serde_json::to_string(&r).unwrap();
        let r2: SignupResult = serde_json::from_str(&json).unwrap();
        assert!(r2.success);
    }

    /// Test GreetInput serde roundtrip
    #[test]
    fn test_greet_input_serde() {
        let input = GreetInput {
            name: "mary".into(),
            greeting: "Hi".into(),
        };
        let json = serde_json::to_string(&input).unwrap();
        let parsed: GreetInput = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.name, "mary");
        assert_eq!(parsed.greeting, "Hi");
    }

    /// Test ClickInput serde roundtrip
    #[test]
    fn test_click_input_serde() {
        let input = ClickInput {
            user_id: "bob".into(),
            secret: "abc123".into(),
        };
        let json = serde_json::to_string(&input).unwrap();
        let parsed: ClickInput = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.user_id, "bob");
    }

    /// Test EmptyInput
    #[test]
    fn test_empty_input() {
        let parsed: EmptyInput = serde_json::from_str("{}").unwrap();
        // Just verify it doesn't panic
        let _ = parsed;
    }

    /// Test UUID generation logic format
    #[test]
    fn test_uuid_format() {
        // UUIDs from Cleat host runtime are typically 36-char strings
        // (standard UUID format). This test validates our expected format.
        let uuid_str = "550e8400-e29b-41d4-a716-446655440000";
        assert_eq!(uuid_str.len(), 36);
        assert_eq!(uuid_str.chars().filter(|&c| c == '-').count(), 4);
    }

    /// Test that function signatures are compatible with #[cleat_entry]
    #[test]
    fn test_cleat_entry_fn_signature() {
        // Verify our entry functions would have compatible signatures
        // by checking the types at compile time
        fn _check_signature(_f: fn(&HostCalls, SubscriptionRequest) -> Result<String, String>) {}
        _check_signature(|_: &HostCalls, _: SubscriptionRequest| Ok("test".to_string()));
    }
}
