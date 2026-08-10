// Package crosslang implements a PlaceOrder workflow that matches the
// Rust place_order entry point in examples/rust-workflow/src/lib.rs.
//
// Both produce the same sequence of host calls (interleaved with DurableLog):
//   - DurableLog "Starting order for user ..."
//   - inventory.Reserve
//   - DurableLog "Reserved inventory: ..."
//   - payments.Charge
//   - DurableLog "Payment processed: ..."
//   - shipping.CreateShipment
//   - DurableLog "Order complete: ..."
//   - notifications.SendEmail
//
// This enables cross-language replay: event history from either language
// can be replayed against the other language's WASM binary.
package crosslang

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// CartItem matches the Rust CartItem struct (snake_case JSON tags).
type CartItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// PlaceOrderInput matches the Rust PlaceOrderInput struct.
type PlaceOrderInput struct {
	UserID string     `json:"user_id"`
	Cart   []CartItem `json:"cart"`
}

// PlaceOrder implements the same logic as the Rust place_order entry point.
func PlaceOrder(h cleat.HostCalls, input PlaceOrderInput) (string, error) {
	if len(input.Cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	h.DurableLog(fmt.Sprintf("Starting order for user %s", input.UserID))

	// Step 1: Reserve inventory.
	reserveReq, _ := json.Marshal(map[string]interface{}{
		"user_id": input.UserID,
		"cart":    input.Cart,
	})
	reserveResp, err := h.DurableCall("inventory", "Reserve", string(reserveReq))
	if err != nil {
		return "", fmt.Errorf("inventory reserve failed: %w", err)
	}
	var reservation struct {
		ReservationID string `json:"reservation_id"`
		TotalCents    int    `json:"total_cents"`
	}
	if err := json.Unmarshal([]byte(reserveResp), &reservation); err != nil {
		return "", fmt.Errorf("bad reservation response: %w", err)
	}
	h.DurableLog(fmt.Sprintf("Reserved inventory: %s", reservation.ReservationID))

	// Step 2: Process payment.
	paymentReq, _ := json.Marshal(map[string]interface{}{
		"user_id":      input.UserID,
		"amount_cents": reservation.TotalCents,
	})
	paymentResp, err := h.DurableCall("payments", "Charge", string(paymentReq))
	if err != nil {
		// Compensate: release reservation.
		releaseReq, _ := json.Marshal(map[string]string{"reservation_id": reservation.ReservationID})
		h.DurableCall("inventory", "Release", string(releaseReq))
		return "", fmt.Errorf("payment failed: %w", err)
	}
	var charge struct {
		ChargeID string `json:"charge_id"`
		Amount   int    `json:"amount"`
	}
	if err := json.Unmarshal([]byte(paymentResp), &charge); err != nil {
		return "", fmt.Errorf("bad charge response: %w", err)
	}
	h.DurableLog(fmt.Sprintf("Payment processed: %s", charge.ChargeID))

	// Step 3: Fulfill order.
	fulfillReq, _ := json.Marshal(map[string]string{
		"reservation_id": reservation.ReservationID,
		"charge_id":      charge.ChargeID,
	})
	trackingResp, err := h.DurableCall("shipping", "CreateShipment", string(fulfillReq))
	if err != nil {
		// Compensate: refund + release.
		refundReq, _ := json.Marshal(map[string]string{"charge_id": charge.ChargeID})
		h.DurableCall("payments", "Refund", string(refundReq))
		releaseReq, _ := json.Marshal(map[string]string{"reservation_id": reservation.ReservationID})
		h.DurableCall("inventory", "Release", string(releaseReq))
		return "", fmt.Errorf("fulfillment failed: %w", err)
	}
	var tracking struct {
		TrackingID string `json:"tracking_id"`
	}
	if err := json.Unmarshal([]byte(trackingResp), &tracking); err != nil {
		return "", fmt.Errorf("bad tracking response: %w", err)
	}
	h.DurableLog(fmt.Sprintf("Order complete: %s", tracking.TrackingID))

	// Step 4: Notify customer (best-effort).
	notifyReq, _ := json.Marshal(map[string]string{
		"user_id":     input.UserID,
		"tracking_id": tracking.TrackingID,
	})
	h.DurableCall("notifications", "SendEmail", string(notifyReq))

	return fmt.Sprintf(`{"tracking_id":%q}`, tracking.TrackingID), nil
}
