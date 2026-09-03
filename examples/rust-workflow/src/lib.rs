// Rust workflow example for cleat durable execution.
// Uses the cleat-sdk crate and #[cleat_entry] proc-macro.
//
// Compiled to WASM with: cargo build --target wasm32-wasip1 --release

use cleat_sdk::HostCalls;
use cleat_macro::cleat_entry;
use serde::{Deserialize, Serialize};

/// Cart item matching the Go CartItem struct.
#[derive(Debug, Serialize, Deserialize)]
struct CartItem {
    sku: String,
    quantity: i32,
}

#[derive(Debug, Serialize, Deserialize)]
struct Reservation {
    reservation_id: String,
    total_cents: i64,
}

#[derive(Debug, Serialize, Deserialize)]
struct Charge {
    charge_id: String,
    #[serde(default)]
    amount: i64,
}

#[derive(Debug, Serialize, Deserialize)]
struct PlaceOrderInput {
    user_id: String,
    cart: Vec<CartItem>,
}

/// Place an order: validate inventory, process payment, fulfill, notify.
/// The fulfilment service's tracking response, and the workflow's result.
///
/// Serialize as well as Deserialize because place_order returns it: the
/// boundary contract is a string containing a JSON-encoded object, and letting
/// serde produce that from a typed value is how Rust satisfies it in one step.
#[derive(Serialize, Deserialize)]
struct Tracking {
    tracking_id: String,
}

#[cleat_entry]
fn place_order(h: &HostCalls, input: PlaceOrderInput) -> Result<Tracking, String> {
    if input.cart.is_empty() {
        return Err("cart is empty".to_string());
    }

    h.cleat_log(&format!("Starting order for user {}", input.user_id));

    // Step 1: Validate and reserve inventory
    let reserve_input = serde_json::json!({
        "user_id": input.user_id,
        "cart": input.cart,
    });
    let (reservation_json, err) = h.cleat_call(
        "inventory", "Reserve", &reserve_input.to_string(),
    );
    if let Some(e) = err {
        return Err(format!("inventory reserve failed: {}", e));
    }
    let reservation: Reservation = serde_json::from_str(&reservation_json)
        .map_err(|e| format!("bad reservation response: {}", e))?;
    h.cleat_log(&format!("Reserved inventory: {}", reservation.reservation_id));

    // Step 2: Process payment
    let payment_input = serde_json::json!({
        "user_id": input.user_id,
        "amount_cents": reservation.total_cents,
    });
    let (charge_json, err) = h.cleat_call(
        "payments", "Charge", &payment_input.to_string(),
    );
    if let Some(e) = err {
        // Compensate: release reservation
        h.cleat_call("inventory", "Release",
            &serde_json::json!({"reservation_id": &reservation.reservation_id}).to_string());
        return Err(format!("payment failed: {}", e));
    }
    let charge: Charge = serde_json::from_str(&charge_json)
        .map_err(|e| format!("bad charge response: {}", e))?;
    h.cleat_log(&format!("Payment processed: {}", charge.charge_id));

    // Step 3: Fulfill order
    let fulfillment_input = serde_json::json!({
        "reservation_id": reservation.reservation_id,
        "charge_id": charge.charge_id,
    });
    let (tracking_json, err) = h.cleat_call(
        "shipping", "CreateShipment", &fulfillment_input.to_string(),
    );
    if let Some(e) = err {
        // Compensate: refund + release
        h.cleat_call("payments", "Refund",
            &serde_json::json!({"charge_id": &charge.charge_id}).to_string());
        h.cleat_call("inventory", "Release",
            &serde_json::json!({"reservation_id": &reservation.reservation_id}).to_string());
        return Err(format!("fulfillment failed: {}", e));
    }

    let tracking: Tracking = serde_json::from_str(&tracking_json)
        .map_err(|e| format!("bad tracking response: {}", e))?;

    // Step 4: Notify customer (best-effort)
    h.cleat_call("notifications", "SendEmail",
        &serde_json::json!({"user_id": input.user_id, "tracking_id": tracking.tracking_id}).to_string());

    h.cleat_log(&format!("Order complete: {}", tracking.tracking_id));

    // Return the object, not the bare id.
    //
    // The boundary contract is a string containing a JSON-encoded object.
    // Ok(tracking.tracking_id) gave serde a String, which it serialised to
    // "TRACK-123456" -- a JSON string, not an object. Go's fixture had the same
    // shape, so the two AGREED by both being wrong and the cross-replay result
    // comparison passed.
    //
    // Rust does not need the SDK changed for this: format_cleat_result already
    // serialises a typed value exactly once, so returning the struct produces
    // the object directly. Go has no typed-result path, so its fixture builds
    // the JSON text and the generated wrapper passes it through. Different
    // routes, identical bytes -- which is what the cross-language comparison
    // checks.
    Ok(tracking)
}

/// A cancellation-aware entry point.
#[cleat_entry]
fn cancel_order(h: &HostCalls, input: PlaceOrderInput) -> Result<String, String> {
    h.cleat_log(&format!("Starting cancelable order for user {}", input.user_id));

    let (cancelled, reason) = h.poll_cancellation();
    if cancelled {
        return Ok(format!("{{\"cancelled\": true, \"reason\": \"{}\"}}", reason));
    }

    h.cleat_log("Cancelable order processing...");
    let (res_json, err) = h.cleat_call("inventory", "Reserve",
        &serde_json::json!({"user_id": input.user_id, "cart": input.cart}).to_string());
    if let Some(e) = err {
        return Err(format!("failed: {}", e));
    }
    Ok(res_json)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_decode_export_result() {
        let (err_code, actual_len) = cleat_sdk::memory::decode_export_result(0x0000_0042_0000_0000);
        assert_eq!(err_code, 0);
        assert_eq!(actual_len, 0x42);
    }

    #[test]
    fn test_encode_export_result() {
        let result = cleat_sdk::memory::encode_export_result(0, 100);
        let (err_code, actual_len) = cleat_sdk::memory::decode_export_result(result as u64);
        assert_eq!(err_code, 0);
        assert_eq!(actual_len, 100);
    }

    #[test]
    fn test_decode_cleat_call_result() {
        let result: i64 = (42u64 << 40) as i64;
        let (response_len, call_error_code, err_code) = cleat_sdk::memory::decode_cleat_call_result(result);
        assert_eq!(response_len, 42);
        assert_eq!(call_error_code, 0);
        assert_eq!(err_code, 0);
    }

    #[test]
    fn test_decode_sleep_result() {
        let result: i64 = ((1u64) << 56 | 5000) as i64;
        let (status, duration) = cleat_sdk::memory::decode_sleep_result(result);
        assert_eq!(status, cleat_sdk::memory::SLEEP_STATUS_SUSPEND);
        assert_eq!(duration, 5000);
    }
}

/// An entry point whose cleanup has a body.
///
/// `h.cleat_defer(description)` registers a description and nothing more: the
/// host records that a defer exists and no code anywhere runs it. That was true
/// of every Rust workflow until IMPROVEMENT-PLAN §3.73, and the SDK documented
/// it as cleanup that runs.
///
/// `defer_func` is the one with a closure attached. The generated
/// `#[cleat_entry]` wrapper drains the table when this returns, so the calls
/// below arrive in the order body, second, first -- LIFO, because a defer
/// releases what the defer before it acquired.
#[cleat_entry]
fn defer_order(h: &HostCalls, input: PlaceOrderInput) -> Result<String, String> {
    let user = input.user_id.clone();
    h.defer_func(move || {
        HostCalls.cleat_call("notifications", "first", &format!("{{\"user\":\"{}\"}}", user));
    });
    h.defer_func(|| {
        HostCalls.cleat_call("notifications", "second", "{}");
    });

    h.cleat_call("inventory", "body", "{}");
    Ok("{\"deferred\":true}".to_string())
}

/// A workflow that suspends the only way this SDK knows how, and nothing else.
///
/// `HostCalls::cleat_sleep_ms`, `await_child` and `await_signals` all suspend by
/// `std::panic::panic_any(SuspendSentinel)`, on the documented understanding
/// that `#[cleat_entry]`'s `catch_unwind` intercepts it and returns
/// `memory::SUSPEND_SENTINEL` to the host. This entry point raises that panic
/// directly, with no host call in the way, so a test can measure whether the
/// interception happens.
///
/// It does not. See `engine/rust_suspend_test.go` and IMPROVEMENT-PLAN 3.87.
/// Keep this entry point: it is the whole of that test's evidence, and it has
/// no other caller.
#[cleat_entry]
fn suspend_probe(_h: &HostCalls, _input: PlaceOrderInput) -> Result<String, String> {
    std::panic::panic_any(cleat_sdk::SuspendSentinel);
}

/// The same suspension, reached through a host call that records it.
///
/// `cleat_sleep_ms` panics with `SuspendSentinel` exactly as `suspend_probe`
/// does, so the guest traps identically -- but the HOST sets session.suspendErr
/// before returning, and the executor lets a suspension win over the error that
/// came with it. The run is therefore reported as a clean suspension.
///
/// Pairing this with `suspend_probe` is what shows the trap is real and merely
/// hidden. Keep both; either alone is misleading. IMPROVEMENT-PLAN 3.87.
#[cleat_entry]
fn sleep_probe(h: &HostCalls, _input: PlaceOrderInput) -> Result<String, String> {
    h.cleat_sleep_ms(300_000);
    Ok("{\"unreachable\":true}".to_string())
}

/// A workflow that exhausts a retry policy through the HOST-side retry loop.
///
/// This is the half of the retry story the Go SDK cannot reach.
/// `HostCalls::cleat_call_with_retry` calls the `cleat_call_retry` import
/// directly, so the loop -- attempts, backoff, exhaustion -- runs on the host
/// inside a single host call, and the whole thing is ONE history event and ONE
/// segment. Go's `DurableCallWithOptions` falls back to an SDK-level loop that
/// backs off with a durable sleep instead, turning an N-attempt policy into N
/// segments; see `engine/retry_backoff_test.go`.
///
/// The error the host mints when the policy exhausts is prefixed
/// `retries exhausted: `, which is exactly the substring the worker's
/// dead-letter predicate matches (`cmd/cleat-worker/setup.go`). So the two SDKs
/// currently disagree about whether an exhausting retry is dead-letterable, and
/// this entry point is the evidence for the Rust side of that.
///
/// Intervals are 1ms because the point is the exhaustion, not the wait. Unlike
/// the Go fixture this does not make the timing load-bearing: the host loop
/// sleeps in-process and never consults a clock the test can race.
///
/// Keep this entry point: it is the whole of `engine/rust_host_retry_test.go`'s
/// evidence and has no other caller.
#[cleat_entry]
fn retry_probe(h: &HostCalls, _input: PlaceOrderInput) -> Result<String, String> {
    let policy = cleat_sdk::RetryPolicy {
        max_attempts: 2,
        initial_interval_ms: 1,
        backoff_multiplier: 1.0,
        maximum_interval_ms: 1,
        non_retryable_errors: vec![],
    };
    let _: serde_json::Value =
        h.cleat_call_with_retry("always-fails", "op", &serde_json::json!({}), &policy)?;
    Ok("{\"unreachable\":true}".to_string())
}
