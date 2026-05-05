// Package autothread demonstrates auto-threading of durable.HostCalls.
//
// Developers declare a package-level var h as a "context object":
//
//	var h durable.HostCalls
//
// Entry points take h as a first parameter (shadows the global). Internal
// helper functions use h directly without declaring it in their signatures.
// The transformer automatically adds h to helper function signatures and
// updates call sites to pass h through.
//
// Build:
//
//	durable build -o /tmp/out ./testdata/autothread/
package autothread

import (
	"fmt"

	"github.com/rcownie/durable/durable"
)

// h is the package-level context object. Functions in the durable closure
// that reference this global will have h durable.HostCalls automatically
// added as a first parameter by the transformer.
var h durable.HostCalls

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

// ---- Entry points (declare h in signature) ----

func PlaceOrder(h durable.HostCalls, userID string, cart []CartItem) (string, error) {
	if len(cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	reservation, err := validateAndReserve(userID, cart)
	if err != nil {
		return "", fmt.Errorf("inventory step failed: %w", err)
	}

	charge, err := processPayment(userID, reservation.TotalCents)
	if err != nil {
		releaseReservation(reservation.ReservationID)
		return "", fmt.Errorf("payment failed: %w", err)
	}

	trackingID, err := fulfillOrder(reservation, charge)
	if err != nil {
		refundPayment(charge.ChargeID)
		releaseReservation(reservation.ReservationID)
		return "", fmt.Errorf("fulfillment failed: %w", err)
	}

	_ = notifyCustomer(userID, trackingID)
	return trackingID, nil
}

func CancelOrder(h durable.HostCalls, orderID string) error {
	_ = refundPayment(orderID)
	return releaseReservation(orderID)
}

// ---- Helper functions (use global h — transformer adds h param) ----

func validateAndReserve(userID string, cart []CartItem) (Reservation, error) {
	for _, item := range cart {
		if err := checkItemAvailability(item.SKU); err != nil {
			return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}
	return reserveInventory(userID, cart)
}

func processPayment(userID string, amountCents int) (Charge, error) {
	pm, err := getDefaultPaymentMethod(userID)
	if err != nil {
		return Charge{}, err
	}
	return chargeCustomer(pm.Token, amountCents)
}

func fulfillOrder(r Reservation, c Charge) (string, error) {
	request := fmt.Sprintf(`{"reservation_id":"%s","charge_id":"%s"}`,
		r.ReservationID, c.ChargeID)
	response, err := h.DurableCall("shipping", "CreateShipment", request)
	if err != nil {
		return "", err
	}
	_ = response
	return "TRACK-123456", nil
}

// ---- Leaf functions that call HostCalls methods ----

func checkItemAvailability(sku string) error {
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

type PaymentMethod struct {
	Token string `json:"token"`
	Type  string `json:"type"`
}

func getDefaultPaymentMethod(userID string) (PaymentMethod, error) {
	request := fmt.Sprintf(`{"user_id":"%s"}`, userID)
	response, err := h.DurableCall("payments", "GetDefaultMethod", request)
	if err != nil {
		return PaymentMethod{}, err
	}
	_ = response
	return PaymentMethod{Token: "pm_tok_555", Type: "card"}, nil
}

func reserveInventory(userID string, items []CartItem) (Reservation, error) {
	request := fmt.Sprintf(`{"user_id":"%s","item_count":%d}`, userID, len(items))
	response, err := h.DurableCall("inventory", "Reserve", request)
	if err != nil {
		return Reservation{}, err
	}
	_ = response
	return Reservation{ReservationID: "resv_abc123", TotalCents: 3299}, nil
}

func chargeCustomer(token string, amountCents int) (Charge, error) {
	request := fmt.Sprintf(`{"token":"%s","amount_cents":%d}`, token, amountCents)
	response, err := h.DurableCall("payments", "Charge", request)
	if err != nil {
		return Charge{}, err
	}
	_ = response
	return Charge{ChargeID: "chg_xyz789", Amount: amountCents}, nil
}

func releaseReservation(reservationID string) error {
	request := fmt.Sprintf(`{"reservation_id":"%s"}`, reservationID)
	_, err := h.DurableCall("inventory", "Release", request)
	return err
}

func refundPayment(chargeID string) error {
	request := fmt.Sprintf(`{"charge_id":"%s"}`, chargeID)
	_, err := h.DurableCall("payments", "Refund", request)
	return err
}

func notifyCustomer(userID, trackingID string) error {
	request := fmt.Sprintf(`{"user_id":"%s","tracking_id":"%s"}`, userID, trackingID)
	_, err := h.DurableCall("notifications", "SendEmail", request)
	return err
}
