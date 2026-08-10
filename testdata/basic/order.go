// Package basic demonstrates a simple durable workflow for testing
// the transformer's analysis pipeline (Phases 1-4).
//
// It defines a PlaceOrder workflow with nested helper functions,
// compensation logic, and multiple HostCalls method usages.
//
// NOTE: This test fixture exercises the low-level DurableCall API.
// Production code should prefer DurableCallTyped which handles JSON
// marshaling/unmarshaling automatically and eliminates injection risks.
package basic

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// ---- Domain types ----

type CartItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type Reservation struct {
	ReservationID string `json:"reservation_id"`
	TotalCents    int    `json:"total_cents"`
}

type Charge struct {
	ChargeID string `json:"charge_id"`
	Amount   int    `json:"amount"`
}

// ---- Entry points ----

// PlaceOrder is the main workflow entry point.
func PlaceOrder(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {
	if len(cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	reservation, err := validateAndReserve(h, userID, cart)
	if err != nil {
		return "", fmt.Errorf("inventory step failed: %w", err)
	}

	charge, err := processPayment(h, userID, reservation.TotalCents)
	if err != nil {
		releaseReservation(h, reservation.ReservationID)
		return "", fmt.Errorf("payment failed: %w", err)
	}

	trackingID, err := fulfillOrder(h, reservation, charge)
	if err != nil {
		refundPayment(h, charge.ChargeID)
		releaseReservation(h, reservation.ReservationID)
		return "", fmt.Errorf("fulfillment failed: %w", err)
	}

	_ = notifyCustomer(h, userID, trackingID)
	return fmt.Sprintf(`{"tracking_id":%q}`, trackingID), nil
}

// CancelOrder is an entry point for cancelling an active order.
func CancelOrder(h cleat.HostCalls, orderID string) error {
	_ = refundPayment(h, orderID)
	return releaseReservation(h, orderID)
}

// ---- Library helpers ----

func validateAndReserve(h cleat.HostCalls, userID string, cart []CartItem) (Reservation, error) {
	for _, item := range cart {
		if err := checkItemAvailability(h, item.SKU); err != nil {
			return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}
	return reserveInventory(h, userID, cart)
}

func checkItemAvailability(h cleat.HostCalls, sku string) error {
	req, _ := json.Marshal(map[string]string{"sku": sku})
	response, err := h.DurableCall("catalog", "LookupItem", string(req))
	if err != nil {
		return err
	}
	if response == "" {
		return fmt.Errorf("SKU %s not found", sku)
	}
	return nil
}

func processPayment(h cleat.HostCalls, userID string, amountCents int) (Charge, error) {
	pm, err := getDefaultPaymentMethod(h, userID)
	if err != nil {
		return Charge{}, err
	}
	return chargeCustomer(h, pm.Token, amountCents)
}

type PaymentMethod struct {
	Token string `json:"token"`
	Type  string `json:"type"`
}

func getDefaultPaymentMethod(h cleat.HostCalls, userID string) (PaymentMethod, error) {
	req, _ := json.Marshal(map[string]string{"user_id": userID})
	response, err := h.DurableCall("payments", "GetDefaultMethod", string(req))
	if err != nil {
		return PaymentMethod{}, err
	}
	_ = response
	return PaymentMethod{Token: "pm_tok_555", Type: "card"}, nil
}

func fulfillOrder(h cleat.HostCalls, r Reservation, c Charge) (string, error) {
	req, _ := json.Marshal(map[string]string{
		"reservation_id": r.ReservationID,
		"charge_id":      c.ChargeID,
	})
	response, err := h.DurableCall("shipping", "CreateShipment", string(req))
	if err != nil {
		return "", err
	}
	_ = response
	return "TRACK-123456", nil
}

// ---- Leaf API calls ----

func reserveInventory(h cleat.HostCalls, userID string, items []CartItem) (Reservation, error) {
	req, _ := json.Marshal(map[string]interface{}{
		"user_id":    userID,
		"item_count": len(items),
	})
	response, err := h.DurableCall("inventory", "Reserve", string(req))
	if err != nil {
		return Reservation{}, err
	}
	_ = response
	return Reservation{ReservationID: "resv_abc123", TotalCents: 3299}, nil
}

func chargeCustomer(h cleat.HostCalls, token string, amountCents int) (Charge, error) {
	req, _ := json.Marshal(map[string]interface{}{
		"token":        token,
		"amount_cents": amountCents,
	})
	response, err := h.DurableCall("payments", "Charge", string(req))
	if err != nil {
		return Charge{}, err
	}
	_ = response
	return Charge{ChargeID: "chg_xyz789", Amount: amountCents}, nil
}

func releaseReservation(h cleat.HostCalls, reservationID string) error {
	req, _ := json.Marshal(map[string]string{"reservation_id": reservationID})
	_, err := h.DurableCall("inventory", "Release", string(req))
	return err
}

func refundPayment(h cleat.HostCalls, chargeID string) error {
	req, _ := json.Marshal(map[string]string{"charge_id": chargeID})
	_, err := h.DurableCall("payments", "Refund", string(req))
	return err
}

// LongRunning performs many HostCalls in a tight loop to burn wall-clock time
// without triggering WASM suspension. Each DurableCall is sub-millisecond so
// the engine keeps executing -- the context deadline is the only exit path.
//
// The operation name must be non-empty. This loop used to call
// DurableCall("noop", "", ""), and the host rejected every one of those on the
// spot: service and operation names are validated against [a-zA-Z0-9._-]+ (see
// engine/memory.go validServiceName), so an empty operation is a malformed
// call target, not a call to an unnamed method. The loop body therefore never
// ran -- the first iteration returned an error and LongRunning bailed out in
// ~200ms regardless of `iterations`, which silently made this fixture useless
// for the one thing it exists to do. See IMPROVEMENT-PLAN.md 2.10.
func LongRunning(h cleat.HostCalls, iterations int) (string, error) {
	for i := 0; i < iterations; i++ {
		if _, err := h.DurableCall("noop", "Noop", "{}"); err != nil {
			return "", err
		}
	}
	return "done", nil
}

func notifyCustomer(h cleat.HostCalls, userID, trackingID string) error {
	req, _ := json.Marshal(map[string]string{
		"user_id":     userID,
		"tracking_id": trackingID,
	})
	_, err := h.DurableCall("notifications", "SendEmail", string(req))
	return err
}
