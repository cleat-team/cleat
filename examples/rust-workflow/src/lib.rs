// Rust workflow example for cleat durable execution.
// This workflow mirrors the Go testdata/basic/order.go PlaceOrder workflow.
//
// Compiled to WASM with: cargo build --target wasm32-wasip1 --release
// The resulting .wasm file can be loaded by the cleat host runtime.

mod host_calls;
mod memory;

use host_calls::HostCalls;
use serde::{Deserialize, Serialize};

/// Cart item matching the Go CartItem struct.
#[derive(Debug, Serialize, Deserialize)]
struct CartItem {
    sku: String,
    quantity: i32,
}

/// Inventory reservation result.
#[derive(Debug, Serialize, Deserialize)]
struct Reservation {
    reservation_id: String,
    total_cents: i64,
}

/// Payment charge result.
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
/// This is the main workflow entry point.
fn place_order_inner(h: &HostCalls, input: &PlaceOrderInput) -> Result<String, String> {
    if input.cart.is_empty() {
        return Err("cart is empty".to_string());
    }

    h.durable_log(&format!("Starting order for user {}", input.user_id));

    // Step 1: Validate and reserve inventory
    let reserve_input = serde_json::json!({
        "user_id": input.user_id,
        "cart": input.cart,
    });
    let (reservation_json, err) = h.durable_call(
        "inventory",
        "Reserve",
        &reserve_input.to_string(),
    );
    if let Some(e) = err {
        return Err(format!("inventory reserve failed: {}", e));
    }
    let reservation: Reservation = serde_json::from_str(&reservation_json)
        .map_err(|e| format!("bad reservation response: {}", e))?;

    h.durable_log(&format!("Reserved inventory: {}", reservation.reservation_id));

    // Step 2: Process payment
    let payment_input = serde_json::json!({
        "user_id": input.user_id,
        "amount_cents": reservation.total_cents,
    });
    let (charge_json, err) = h.durable_call(
        "payments",
        "Charge",
        &payment_input.to_string(),
    );
    if let Some(e) = err {
        // Compensate: release reservation
        let release_input = serde_json::json!({"reservation_id": &reservation.reservation_id});
        h.durable_call("inventory", "Release", &release_input.to_string());
        return Err(format!("payment failed: {}", e));
    }
    let charge: Charge = serde_json::from_str(&charge_json)
        .map_err(|e| format!("bad charge response: {}", e))?;

    h.durable_log(&format!("Payment processed: {}", charge.charge_id));

    // Step 3: Fulfill order
    let fulfillment_input = serde_json::json!({
        "reservation_id": reservation.reservation_id,
        "charge_id": charge.charge_id,
    });
    let (tracking_json, err) = h.durable_call(
        "shipping",
        "CreateShipment",
        &fulfillment_input.to_string(),
    );
    if let Some(e) = err {
        // Compensate: refund + release
        let refund_input = serde_json::json!({"charge_id": &charge.charge_id});
        h.durable_call("payments", "Refund", &refund_input.to_string());
        let release_input = serde_json::json!({"reservation_id": &reservation.reservation_id});
        h.durable_call("inventory", "Release", &release_input.to_string());
        return Err(format!("fulfillment failed: {}", e));
    }

    #[derive(Deserialize)]
    struct Tracking { tracking_id: String }
    let tracking: Tracking = serde_json::from_str(&tracking_json)
        .map_err(|e| format!("bad tracking response: {}", e))?;

    // Step 4: Notify customer (best-effort)
    let notify_input = serde_json::json!({
        "user_id": input.user_id,
        "tracking_id": tracking.tracking_id,
    });
    let (_, _) = h.durable_call("notifications", "SendEmail", &notify_input.to_string());

    h.durable_log(&format!("Order complete: {}", tracking.tracking_id));
    Ok(tracking.tracking_id)
}

/// WASM export: place_order
///
/// ABI: (args_ptr: i32, args_len: i32, out_ptr: i32, max_out_len: i32) -> i64
/// - Input: JSON-serialized PlaceOrderInput at args_ptr with args_len bytes
/// - Output: JSON-serialized result written to out_ptr, up to max_out_len bytes
/// - Return: bits 0-31 = errCode (0 = success), bits 32-63 = actual output length
/// - Suspend sentinel: 1 << 62 = 0x4000000000000000
///
/// Matches the export convention from internal/host/runtime.go CallExport.
#[no_mangle]
pub unsafe extern "C" fn place_order(
    args_ptr: *const u8,
    args_len: u32,
    out_ptr: *mut u8,
    max_out_len: u32,
) -> i64 {
    let args_json = unsafe { memory::read_string(args_ptr, args_len) };

    let input: PlaceOrderInput = match serde_json::from_str(&args_json) {
        Ok(v) => v,
        Err(e) => {
            let err_msg = format!("{{\"error\": \"{}\"}}", e);
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &err_msg) };
            return memory::encode_export_result(1, n);
        }
    };

    let h = HostCalls;

    match place_order_inner(&h, &input) {
        Ok(tracking_id) => {
            let result_json = serde_json::json!({"tracking_id": tracking_id}).to_string();
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &result_json) };
            memory::encode_export_result(0, n)
        }
        Err(e) => {
            let err_json = serde_json::json!({"error": e}).to_string();
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &err_json) };
            memory::encode_export_result(1, n)
        }
    }
}

/// WASM export: cancel_order
/// A cancellation-aware entry point that polls for cancellation between steps.
#[no_mangle]
pub unsafe extern "C" fn cancel_order(
    args_ptr: *const u8,
    args_len: u32,
    out_ptr: *mut u8,
    max_out_len: u32,
) -> i64 {
    let args_json = unsafe { memory::read_string(args_ptr, args_len) };
    let input: PlaceOrderInput = match serde_json::from_str(&args_json) {
        Ok(v) => v,
        Err(e) => {
            let err_msg = format!("{{\"error\": \"{}\"}}", e);
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &err_msg) };
            return memory::encode_export_result(1, n);
        }
    };

    let h = HostCalls;
    h.durable_log(&format!("Starting cancelable order for user {}", input.user_id));

    // Check cancellation before each step
    let (cancelled, reason) = h.poll_cancellation();
    if cancelled {
        let n = unsafe { memory::write_string(out_ptr, max_out_len, &format!("{{\"cancelled\": true, \"reason\": \"{}\"}}", reason)) };
        return memory::encode_export_result(0, n);
    }

    match place_order_inner(&h, &input) {
        Ok(tracking_id) => {
            let result_json = serde_json::json!({"tracking_id": tracking_id}).to_string();
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &result_json) };
            memory::encode_export_result(0, n)
        }
        Err(e) => {
            let err_json = serde_json::json!({"error": e}).to_string();
            let n = unsafe { memory::write_string(out_ptr, max_out_len, &err_json) };
            memory::encode_export_result(1, n)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_memory_decode_export_result() {
        let (err_code, actual_len) = memory::decode_export_result(0x0000_0042_0000_0000);
        assert_eq!(err_code, 0);
        assert_eq!(actual_len, 0x42);
    }

    #[test]
    fn test_memory_encode_export_result() {
        let result = memory::encode_export_result(0, 100);
        let (err_code, actual_len) = memory::decode_export_result(result as u64);
        assert_eq!(err_code, 0);
        assert_eq!(actual_len, 100);
    }

    #[test]
    fn test_memory_decode_durable_call_result() {
        // Simulate: responseLen=42, callErrorCode=0, errCode=0
        let result: i64 = (42u64 << 40) as i64;
        let (response_len, call_error_code, err_code) = memory::decode_durable_call_result(result);
        assert_eq!(response_len, 42);
        assert_eq!(call_error_code, 0);
        assert_eq!(err_code, 0);
    }

    #[test]
    fn test_memory_decode_sleep_result() {
        let result: i64 = ((1u64) << 56 | 5000) as i64;
        let (status, duration) = memory::decode_sleep_result(result);
        assert_eq!(status, memory::SLEEP_STATUS_SUSPEND);
        assert_eq!(duration, 5000);
    }

    #[test]
    fn test_memory_decode_await_signals() {
        let result: i64 = ((5u64) << 48 | (10u64) << 32 | (1u64) << 16) as i64;
        let (sig_name_len, payload_len, timed_out, err_code) = memory::decode_await_signals_result(result);
        assert_eq!(sig_name_len, 5);
        assert_eq!(payload_len, 10);
        assert!(timed_out);
        assert_eq!(err_code, 0);
    }
}
