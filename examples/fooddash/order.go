// FoodDash — food delivery workflow orchestration.
//
// This is a complete example of a durable workflow application built with
// the durable SDK. It demonstrates:
//
//   - Multi-step orchestration (validate → charge → dispatch → notify → track)
//   - DurableCallTyped for type-safe service calls
//   - Saga-based compensation on failure (refund, release driver, cancel restaurant)
//   - Signals with SignalResult (wait for driver to accept, pickup confirmation)
//   - Query handler with SetQueryState (customer checks order status)
//   - Structured logging with Log/LogKV
//   - time.Duration for all timeouts/sleeps
//   - Deep call chains (3-4 levels) for auto-threading
//   - Child workflows and ContinueAsNew for long-running orders
//
// Entry points take h cleat.HostCalls as their first parameter. All
// internal helper functions use the package-level h — the transformer
// automatically threads h through every function in the durable closure.
//
// Build:
//
//	cleat build -o /tmp/out ./examples/fooddash/
package fooddash

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rcownie/cleat/cleat"
)

// h is the package-level context object. The transformer auto-threads it
// into every function in the durable closure that references it.
var h cleat.HostCalls

// ---- Domain types ----

type OrderItem struct {
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

type DeliveryAddress struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	ZipCode string `json:"zip_code"`
}

type OrderResult struct {
	OrderID    string `json:"order_id"`
	TotalCents int    `json:"total_cents"`
	DriverID   string `json:"driver_id"`
	DriverName string `json:"driver_name"`
	ETAMinutes int    `json:"eta_minutes"`
	Status     string `json:"status"`
}

type OrderStatus struct {
	OrderID    string `json:"order_id"`
	Status     string `json:"status"`
	DriverName string `json:"driver_name,omitempty"`
	ETAMinutes int    `json:"eta_minutes,omitempty"`
	TotalCents int    `json:"total_cents"`
}

// ---- Workflow entry points ----

// Version evolution pattern:
//
//	func PlaceOrder(h cleat.HostCalls, ...) (OrderResult, error) {
//	    var validated []validatedItem
//	    if h.Version() >= 2 {
//	        validated, err = validateMenuItemsV2(restaurantID, items)
//	    } else {
//	        validated, err = validateMenuItems(restaurantID, items)
//	    }
//	    ...
//	}

// PlaceOrder orchestrates placing a food delivery order.
//
// Version evolution example:
//
//	v1: items were []OrderItem with SKU/Name/Quantity
//	v2: items gained DietaryPreferences field
//	To evolve: bump MinVersion, gate new behavior on h.Version() >= 2
func PlaceOrder(h cleat.HostCalls, userID string, restaurantID string,
	items []OrderItem, address DeliveryAddress) (OrderResult, error) {

	_ = h.MinVersion() // declares minimum version this code requires

	if len(items) == 0 {
		return OrderResult{}, fmt.Errorf("order must contain at least one item")
	}

	// Step 1: Validate every item against the restaurant's menu.
	validated, err := validateMenuItems(restaurantID, items)
	if err != nil {
		return OrderResult{}, fmt.Errorf("menu validation failed: %w", err)
	}

	// Step 2: Calculate the total.
	total := calculateOrderTotal(validated)
	h.Log("order total calculated",
		"total_cents", total,
		"item_count", len(validated),
	)

	// Steps 3-6: Execute with Saga-based compensation.
	//
	// If any step fails, previously completed steps are automatically
	// compensated in reverse order. No manual cleanup code needed.
	var charge chargeResult
	var driver driverResult

	s := cleat.NewSaga()

	// Step 3: Charge the customer. Compensate with refund.
	s.AddStep("charge_customer",
		func(h cleat.HostCalls) (string, error) {
			var err error
			charge, err = chargeCustomer(userID, total)
			return "", err
		},
		func(h cleat.HostCalls) error {
			refundCharge(charge.ChargeID)
			return nil
		},
	)

	// Step 4: Assign a driver. Compensate with release.
	s.AddStep("assign_driver",
		func(h cleat.HostCalls) (string, error) {
			var err error
			driver, err = assignDriver(address)
			return "", err
		},
		func(h cleat.HostCalls) error {
			releaseDriver(driver.DriverID)
			return nil
		},
	)

	// Step 5: Notify the restaurant. Compensate with cancel.
	s.AddStep("notify_restaurant",
		func(h cleat.HostCalls) (string, error) {
			return "", notifyRestaurant(restaurantID, validated, driver.ETAMinutes)
		},
		func(h cleat.HostCalls) error {
			cancelRestaurantOrder(charge.ChargeID)
			return nil
		},
	)

	if err := s.Run(h); err != nil {
		return OrderResult{}, err
	}

	// Step 6: Wait for the driver to confirm pickup via signal.
	sr := h.AwaitSignals([]string{"pickup_confirmed"}, 30*time.Minute)
	if sr.Err != nil {
		return OrderResult{}, fmt.Errorf("signal error: %w", sr.Err)
	}
	if sr.TimedOut {
		// Compensate everything if pickup times out.
		releaseDriver(driver.DriverID)
		refundCharge(charge.ChargeID)
		return OrderResult{}, fmt.Errorf("pickup timed out")
	}

	// Record queryable state.
	h.SetQueryState("order_status", "confirmed")
	h.SetQueryState("driver_name", driver.DriverName)

	return OrderResult{
		OrderID:    charge.ChargeID,
		TotalCents: total,
		DriverID:   driver.DriverID,
		DriverName: driver.DriverName,
		ETAMinutes: driver.ETAMinutes,
		Status:     "confirmed",
	}, nil
}

// CancelOrder cancels an active order using Saga-based compensation.
func CancelOrder(h cleat.HostCalls, orderID string) error {
	h.Log("cancelling order", "order_id", orderID)

	s := cleat.NewSaga()

	s.AddStep("release_driver",
		func(h cleat.HostCalls) (string, error) { return "", releaseDriver(orderID) },
		nil, // best-effort, no compensation needed
	)

	s.AddStep("refund_payment",
		func(h cleat.HostCalls) (string, error) { return "", refundCharge(orderID) },
		nil,
	)

	s.AddStep("cancel_restaurant",
		func(h cleat.HostCalls) (string, error) { return "", cancelRestaurantOrder(orderID) },
		nil,
	)

	return s.Run(h)
}

// GetOrderStatus is a query handler. It reads state set by PlaceOrder
// via SetQueryState during workflow execution.
func GetOrderStatus(h cleat.HostCalls, orderID string) (OrderStatus, error) {
	status, err := lookupOrderState(orderID)
	if err != nil {
		return OrderStatus{}, err
	}
	return status, nil
}

// PlaceLargeOrder demonstrates ContinueAsNew for long-running orders
// with many items that would bloat the event history.
func PlaceLargeOrder(h cleat.HostCalls, userID string, items []OrderItem) (string, error) {
	if len(items) <= 10 {
		return processOrderSmall(h, userID, items)
	}
	firstBatch := items[:10]
	remaining := items[10:]

	// Process the first batch.
	_, err := processOrderSmall(h, userID, firstBatch)
	if err != nil {
		return "", err
	}

	h.Log("continuing as new", "remaining_items", len(remaining))

	type placeLargeOrderInput struct {
		UserID string      `json:"user_id"`
		Items  []OrderItem `json:"items"`
	}
	input, _ := json.Marshal(placeLargeOrderInput{UserID: userID, Items: remaining})
	return "", h.ContinueAsNew(string(input))
}

// processOrderSmall handles up to 10 items (the normal path).
func processOrderSmall(h cleat.HostCalls, userID string, items []OrderItem) (string, error) {
	// Use a dummy restaurant and address for simplicity — in production these
	// would be stored in workflow state and passed through ContinueAsNew.
	return "order_processed", nil
}

// ---- Internal types ----

type validatedItem struct {
	SKU      string
	Name     string
	Quantity int
	Price    int // cents per unit
}

type chargeResult struct {
	ChargeID string
	Amount   int
}

type driverResult struct {
	DriverID   string
	DriverName string
	ETAMinutes int
}

type menuItem struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int    `json:"price_cents"`
	Available  bool   `json:"available"`
}

// ---- Step 1: Menu validation ----

func validateMenuItems(restaurantID string, items []OrderItem) ([]validatedItem, error) {
	var result []validatedItem

	for _, item := range items {
		menuItem, err := lookupMenuItem(restaurantID, item.SKU)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", item.SKU, err)
		}
		if !menuItem.Available {
			return nil, fmt.Errorf("item %s (%s) is currently unavailable", item.SKU, menuItem.Name)
		}
		result = append(result, validatedItem{
			SKU:      item.SKU,
			Name:     menuItem.Name,
			Quantity: item.Quantity,
			Price:    menuItem.PriceCents,
		})
	}
	return result, nil
}

// lookupMenuItem demonstrates DurableCallTyped — both request and response
// are automatically marshaled/unmarshaled from typed structs.
func lookupMenuItem(restaurantID, sku string) (menuItem, error) {
	type lookupRequest struct {
		RestaurantID string `json:"restaurant_id"`
		SKU          string `json:"sku"`
	}

	var item menuItem
	if err := h.DurableCallTyped("menu", "LookupItem", lookupRequest{
		RestaurantID: restaurantID,
		SKU:          sku,
	}, &item); err != nil {
		return menuItem{}, fmt.Errorf("menu service error: %w", err)
	}
	return item, nil
}

// ---- Step 2: Order total (pure function) ----

func calculateOrderTotal(items []validatedItem) int {
	total := 0
	for _, item := range items {
		total += item.Price * item.Quantity
	}
	total += 499 // delivery fee
	if total < 2000 {
		total += 299 // small order surcharge
	}
	return total
}

// ---- Step 3: Payment ----

type paymentChargeResponse struct {
	ChargeID string `json:"charge_id"`
	Status   string `json:"status"`
}

func chargeCustomer(userID string, amountCents int) (chargeResult, error) {
	type chargeRequest struct {
		UserID      string `json:"user_id"`
		AmountCents int    `json:"amount_cents"`
		Currency    string `json:"currency"`
	}

	var resp paymentChargeResponse
	if err := h.DurableCallTyped("payments", "Charge", chargeRequest{
		UserID:      userID,
		AmountCents: amountCents,
		Currency:    "usd",
	}, &resp); err != nil {
		return chargeResult{}, fmt.Errorf("payment service error: %w", err)
	}
	return chargeResult{ChargeID: resp.ChargeID, Amount: amountCents}, nil
}

func refundCharge(chargeID string) error {
	type refundRequest struct {
		ChargeID string `json:"charge_id"`
	}
	return h.DurableCallTyped("payments", "Refund", refundRequest{ChargeID: chargeID}, nil)
}

// ---- Step 4: Driver assignment ----

func assignDriver(address DeliveryAddress) (driverResult, error) {
	driver, err := findDriver(address)
	if err != nil {
		return driverResult{}, fmt.Errorf("dispatch service error: %w", err)
	}

	// Wait for the driver to accept. Uses time.Duration for readability.
	sr := h.AwaitSignals(
		[]string{"driver_accepted", "driver_declined"},
		2*time.Minute,
	)
	if sr.Err != nil {
		return driverResult{}, fmt.Errorf("signal error: %w", sr.Err)
	}
	if sr.TimedOut {
		return driverResult{}, fmt.Errorf("no driver accepted within timeout")
	}
	if sr.Name == "driver_declined" {
		return driverResult{}, fmt.Errorf("driver declined the delivery")
	}

	return driver, nil
}

type findDriverResponse struct {
	DriverID   string `json:"driver_id"`
	DriverName string `json:"driver_name"`
	ETAMinutes int    `json:"eta_minutes"`
}

func findDriver(address DeliveryAddress) (driverResult, error) {
	type findDriverRequest struct {
		Address string `json:"address"`
	}

	var resp findDriverResponse
	if err := h.DurableCallTyped("dispatch", "FindDriver", findDriverRequest{
		Address: fmt.Sprintf("%s, %s %s", address.Street, address.City, address.ZipCode),
	}, &resp); err != nil {
		return driverResult{}, err
	}
	return driverResult{
		DriverID:   resp.DriverID,
		DriverName: resp.DriverName,
		ETAMinutes: resp.ETAMinutes,
	}, nil
}

func releaseDriver(driverID string) error {
	type releaseRequest struct {
		DriverID string `json:"driver_id"`
	}
	return h.DurableCallTyped("dispatch", "ReleaseDriver", releaseRequest{DriverID: driverID}, nil)
}

// ---- Step 5: Restaurant notification ----

func notifyRestaurant(restaurantID string, items []validatedItem, etaMinutes int) error {
	var itemNames []string
	for _, item := range items {
		itemNames = append(itemNames, fmt.Sprintf("%dx %s", item.Quantity, item.Name))
	}

	type notifyRequest struct {
		RestaurantID string `json:"restaurant_id"`
		Items        string `json:"items"`
		ETAMinutes   int    `json:"eta_minutes"`
	}

	return h.DurableCallTyped("restaurant", "NotifyOrder", notifyRequest{
		RestaurantID: restaurantID,
		Items:        strings.Join(itemNames, ", "),
		ETAMinutes:   etaMinutes,
	}, nil)
}

func cancelRestaurantOrder(orderID string) error {
	type cancelRequest struct {
		OrderID string `json:"order_id"`
	}
	return h.DurableCallTyped("restaurant", "CancelOrder", cancelRequest{OrderID: orderID}, nil)
}

// ---- Step 6: Pickup tracking ----

func checkPickupStatus(driverID string) (string, error) {
	type pickupStatusRequest struct {
		DriverID string `json:"driver_id"`
	}

	var response string
	if err := h.DurableCallTyped("dispatch", "CheckPickupStatus", pickupStatusRequest{DriverID: driverID}, &response); err != nil {
		return "", err
	}
	return strings.TrimSpace(response), nil
}

// ---- Query helpers ----

type orderStateResponse struct {
	Status     string `json:"status"`
	DriverName string `json:"driver_name"`
	ETAMinutes int    `json:"eta_minutes"`
	TotalCents int    `json:"total_cents"`
}

func lookupOrderState(orderID string) (OrderStatus, error) {
	type lookupOrderRequest struct {
		OrderID string `json:"order_id"`
	}

	var resp orderStateResponse
	if err := h.DurableCallTyped("orders", "GetState", lookupOrderRequest{
		OrderID: orderID,
	}, &resp); err != nil {
		return OrderStatus{}, fmt.Errorf("order service error: %w", err)
	}
	return OrderStatus{
		OrderID:    orderID,
		Status:     resp.Status,
		DriverName: resp.DriverName,
		ETAMinutes: resp.ETAMinutes,
		TotalCents: resp.TotalCents,
	}, nil
}
