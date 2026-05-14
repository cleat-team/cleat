// Package autothread demonstrates auto-threading of cleat.HostCalls.
//
// Developers declare a package-level var h as a "context object":
//
//	var h cleat.HostCalls
//
// Entry points take h as a first parameter (shadows the global). Internal
// helper functions use h directly without declaring it in their signatures.
// The transformer automatically adds h to helper function signatures and
// updates call sites to pass h through.
//
// Build:
//
//	cleat build -o /tmp/out ./testdata/autothread/
//
// NOTE: This fixture uses raw json.Marshal to exercise the low-level
// DurableCall API. Production code should prefer DurableCallTyped.
package autothread

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// h is the package-level context object. Functions in the durable closure
// that reference this global will have h cleat.HostCalls automatically
// added as a first parameter by the transformer.
var h cleat.HostCalls

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

func PlaceOrder(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {
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

func CancelOrder(h cleat.HostCalls, orderID string) error {
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

// ---- Leaf functions that call HostCalls methods ----

func checkItemAvailability(sku string) error {
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

type PaymentMethod struct {
	Token string `json:"token"`
	Type  string `json:"type"`
}

func getDefaultPaymentMethod(userID string) (PaymentMethod, error) {
	req, _ := json.Marshal(map[string]string{"user_id": userID})
	response, err := h.DurableCall("payments", "GetDefaultMethod", string(req))
	if err != nil {
		return PaymentMethod{}, err
	}
	_ = response
	return PaymentMethod{Token: "pm_tok_555", Type: "card"}, nil
}

func reserveInventory(userID string, items []CartItem) (Reservation, error) {
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

func chargeCustomer(token string, amountCents int) (Charge, error) {
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

func releaseReservation(reservationID string) error {
	req, _ := json.Marshal(map[string]string{"reservation_id": reservationID})
	_, err := h.DurableCall("inventory", "Release", string(req))
	return err
}

func refundPayment(chargeID string) error {
	req, _ := json.Marshal(map[string]string{"charge_id": chargeID})
	_, err := h.DurableCall("payments", "Refund", string(req))
	return err
}

func notifyCustomer(userID, trackingID string) error {
	req, _ := json.Marshal(map[string]string{
		"user_id":     userID,
		"tracking_id": trackingID,
	})
	_, err := h.DurableCall("notifications", "SendEmail", string(req))
	return err
}
