// Package basic demonstrates a simple durable workflow for testing
// the transformer's analysis pipeline (Phases 1-4).
//
// It defines a PlaceOrder workflow with nested helper functions,
// compensation logic, and multiple HostCalls method usages.
package basic

import (
	"fmt"

	"github.com/rcownie/durable/durable"
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
func PlaceOrder(h durable.HostCalls, userID string, cart []CartItem) (string, error) {
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
	return trackingID, nil
}

// CancelOrder is an entry point for cancelling an active order.
func CancelOrder(h durable.HostCalls, orderID string) error {
	_ = refundPayment(h, orderID)
	return releaseReservation(h, orderID)
}

// ---- Library helpers ----

func validateAndReserve(h durable.HostCalls, userID string, cart []CartItem) (Reservation, error) {
	for _, item := range cart {
		if err := checkItemAvailability(h, item.SKU); err != nil {
			return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}
	return reserveInventory(h, userID, cart)
}

func checkItemAvailability(h durable.HostCalls, sku string) error {
	request := fmt.Sprintf(`{"sku":"%s"}`, sku)
	response, err := h.DurableCall("catalog", "LookupItem", request)
	if err != nil {
		return err
	}
	if response == "" {
		return fmt.Errorf("SKU %s not found", sku)
	}
	return nil
}

func processPayment(h durable.HostCalls, userID string, amountCents int) (Charge, error) {
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

func getDefaultPaymentMethod(h durable.HostCalls, userID string) (PaymentMethod, error) {
	request := fmt.Sprintf(`{"user_id":"%s"}`, userID)
	response, err := h.DurableCall("payments", "GetDefaultMethod", request)
	if err != nil {
		return PaymentMethod{}, err
	}
	_ = response
	return PaymentMethod{Token: "pm_tok_555", Type: "card"}, nil
}

func fulfillOrder(h durable.HostCalls, r Reservation, c Charge) (string, error) {
	request := fmt.Sprintf(`{"reservation_id":"%s","charge_id":"%s"}`,
		r.ReservationID, c.ChargeID)
	response, err := h.DurableCall("shipping", "CreateShipment", request)
	if err != nil {
		return "", err
	}
	_ = response
	return "TRACK-123456", nil
}

// ---- Leaf API calls ----

func reserveInventory(h durable.HostCalls, userID string, items []CartItem) (Reservation, error) {
	request := fmt.Sprintf(`{"user_id":"%s","item_count":%d}`, userID, len(items))
	response, err := h.DurableCall("inventory", "Reserve", request)
	if err != nil {
		return Reservation{}, err
	}
	_ = response
	return Reservation{ReservationID: "resv_abc123", TotalCents: 3299}, nil
}

func chargeCustomer(h durable.HostCalls, token string, amountCents int) (Charge, error) {
	request := fmt.Sprintf(`{"token":"%s","amount_cents":%d}`, token, amountCents)
	response, err := h.DurableCall("payments", "Charge", request)
	if err != nil {
		return Charge{}, err
	}
	_ = response
	return Charge{ChargeID: "chg_xyz789", Amount: amountCents}, nil
}

func releaseReservation(h durable.HostCalls, reservationID string) error {
	request := fmt.Sprintf(`{"reservation_id":"%s"}`, reservationID)
	_, err := h.DurableCall("inventory", "Release", request)
	return err
}

func refundPayment(h durable.HostCalls, chargeID string) error {
	request := fmt.Sprintf(`{"charge_id":"%s"}`, chargeID)
	_, err := h.DurableCall("payments", "Refund", request)
	return err
}

func notifyCustomer(h durable.HostCalls, userID, trackingID string) error {
	request := fmt.Sprintf(`{"user_id":"%s","tracking_id":"%s"}`, userID, trackingID)
	_, err := h.DurableCall("notifications", "SendEmail", request)
	return err
}
