// Package workflow defines the order processing workflow.
//
// The developer writes this in near-standard Go. All external interactions
// go through the durable.HostCalls interface. The code transformer:
//
//  1. Parses this file with go/parser
//  2. Identifies calls that need durability (anything using HostCalls)
//  3. Generates the WASM-compatible version with //go:wasmimport stubs
//  4. The generated code compiles to WASM and is stored in the cluster DB
//
// Build for WASM:
//
//	GOOS=wasip1 GOARCH=wasm go build -o order.wasm .
//
// Build for native testing:
//
//	go build -tags=native .
package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/rcownie/durable/durable"
)

// Domain types — ordinary Go structs.
type CartItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type Reservation struct {
	ReservationID string `json:"reservation_id"`
	TotalCents    int    `json:"total_cents"`
	Address       string `json:"address"`
}

type Charge struct {
	ChargeID string `json:"charge_id"`
	Amount   int    `json:"amount_cents"`
}

type PaymentMethod struct {
	Token string `json:"token"`
	Type  string `json:"type"`
}

// PlaceOrder is the top-level workflow entry point.
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

func validateAndReserve(h durable.HostCalls, userID string, cart []CartItem) (Reservation, error) {
	for _, item := range cart {
		if err := checkItemAvailability(h, item.SKU); err != nil {
			return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}
	return reserveInventory(h, userID, cart)
}

func checkItemAvailability(h durable.HostCalls, sku string) error {
	type lookupReq struct {
		SKU string `json:"sku"`
	}
	var result struct {
		Found bool `json:"found"`
	}
	if err := h.DurableCallTyped("catalog", "LookupItem", lookupReq{SKU: sku}, &result); err != nil {
		return err
	}
	if !result.Found {
		return fmt.Errorf("SKU %s not found in catalog", sku)
	}
	return nil
}

func reserveInventory(h durable.HostCalls, userID string, items []CartItem) (Reservation, error) {
	type reserveReq struct {
		UserID    string     `json:"user_id"`
		Items     []CartItem `json:"items"`
	}
	_, err := h.DurableCall("inventory", "Reserve", mustMarshal(reserveReq{UserID: userID, Items: items}))
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{
		ReservationID: "resv_abc123",
		TotalCents:    3299,
		Address:       "123 Main St",
	}, nil
}

func processPayment(h durable.HostCalls, userID string, amountCents int) (Charge, error) {
	pm, err := getDefaultPaymentMethod(h, userID)
	if err != nil {
		return Charge{}, err
	}

	type chargeReq struct {
		Token       string `json:"token"`
		AmountCents int    `json:"amount_cents"`
	}
	_, err = h.DurableCall("payments", "Charge", mustMarshal(chargeReq{Token: pm.Token, AmountCents: amountCents}))
	if err != nil {
		return Charge{}, err
	}
	return Charge{ChargeID: "chg_xyz789", Amount: amountCents}, nil
}

func getDefaultPaymentMethod(h durable.HostCalls, userID string) (PaymentMethod, error) {
	type pmReq struct {
		UserID string `json:"user_id"`
	}
	_, err := h.DurableCall("payments", "GetDefaultMethod", mustMarshal(pmReq{UserID: userID}))
	if err != nil {
		return PaymentMethod{}, err
	}
	return PaymentMethod{Token: "pm_tok_555", Type: "card"}, nil
}

func fulfillOrder(h durable.HostCalls, r Reservation, c Charge) (string, error) {
	type shipReq struct {
		ReservationID string `json:"reservation_id"`
		Address       string `json:"address"`
		ChargeID      string `json:"charge_id"`
	}
	_, err := h.DurableCall("shipping", "CreateShipment", mustMarshal(shipReq{
		ReservationID: r.ReservationID,
		Address:       r.Address,
		ChargeID:      c.ChargeID,
	}))
	if err != nil {
		return "", err
	}
	return "TRACK-123456", nil
}

func releaseReservation(h durable.HostCalls, reservationID string) error {
	type releaseReq struct {
		ReservationID string `json:"reservation_id"`
	}
	_, err := h.DurableCall("inventory", "Release", mustMarshal(releaseReq{ReservationID: reservationID}))
	return err
}

func refundPayment(h durable.HostCalls, chargeID string) error {
	type refundReq struct {
		ChargeID string `json:"charge_id"`
	}
	_, err := h.DurableCall("payments", "Refund", mustMarshal(refundReq{ChargeID: chargeID}))
	return err
}

func notifyCustomer(h durable.HostCalls, userID, trackingID string) error {
	type notifyReq struct {
		UserID     string `json:"user_id"`
		TrackingID string `json:"tracking_id"`
	}
	_, err := h.DurableCall("notifications", "SendEmail", mustMarshal(notifyReq{UserID: userID, TrackingID: trackingID}))
	return err
}

func mustMarshal(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal: %v", err))
	}
	return string(data)
}
