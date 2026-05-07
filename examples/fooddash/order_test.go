package fooddash

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcownie/cleat/cleat/cleattest"
)

// setupEnv creates a test environment and wires it into the package-level h
// so both entry-point parameters and helper-function references work.
func setupEnv() *cleattest.TestEnv {
	env := cleattest.NewTestEnv()
	h = env.H()
	return env
}

func TestPlaceOrder_Success(t *testing.T) {
	env := setupEnv()

	// Stub menu lookups for two items.
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "pizza", Name: "Margherita Pizza", PriceCents: 1299, Available: true,
	}, nil)
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "soda", Name: "Cola", PriceCents: 299, Available: true,
	}, nil)

	// Stub payment.
	env.OnCall("payments", "Charge", nil).ReturnJSON(paymentChargeResponse{
		ChargeID: "chg_abc123", Status: "ok",
	}, nil)

	// Stub dispatch.
	env.OnCall("dispatch", "FindDriver", nil).ReturnJSON(struct {
		DriverID   string `json:"driver_id"`
		DriverName string `json:"driver_name"`
		ETAMinutes int    `json:"eta_minutes"`
	}{DriverID: "drv_xyz", DriverName: "Alex", ETAMinutes: 15}, nil)

	// Signal driver acceptance immediately.
	env.Signal("driver_accepted", `{"driver_name":"Alex"}`)

	// Stub restaurant notification.
	env.OnCall("restaurant", "NotifyOrder", nil).Return("{}", nil)

	// Signal pickup confirmation immediately.
	env.Signal("pickup_confirmed", `{"status":"picked_up"}`)

	result, err := PlaceOrder(env.H(), "user_1", "rest_1",
		[]OrderItem{
			{SKU: "pizza", Quantity: 1},
			{SKU: "soda", Quantity: 2},
		},
		DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
	)

	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	if result.Status != "confirmed" {
		t.Errorf("expected status 'confirmed', got %q", result.Status)
	}
	if result.OrderID != "chg_abc123" {
		t.Errorf("expected OrderID 'chg_abc123', got %q", result.OrderID)
	}
	if result.DriverName != "Alex" {
		t.Errorf("expected DriverName 'Alex', got %q", result.DriverName)
	}

	// Verify queryable state was set.
	if status, ok := env.QueryState("order_status"); !ok || status != "confirmed" {
		t.Errorf("expected order_status='confirmed', got ok=%v val=%q", ok, status)
	}

	// Verify expected calls were made.
	env.AssertCalled(t, "menu", "LookupItem")
	env.AssertCalled(t, "payments", "Charge")
	env.AssertCalled(t, "dispatch", "FindDriver")
	env.AssertCalled(t, "restaurant", "NotifyOrder")
}

func TestPlaceOrder_EmptyCart(t *testing.T) {
	env := setupEnv()

	_, err := PlaceOrder(env.H(), "user_1", "rest_1",
		[]OrderItem{},
		DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
	)

	if err == nil {
		t.Fatal("expected error for empty cart, got nil")
	}
	if !strings.Contains(err.Error(), "at least one item") {
		t.Errorf("expected 'at least one item' error, got: %v", err)
	}
}

func TestPlaceOrder_MenuUnavailable(t *testing.T) {
	env := setupEnv()

	// First item lookup succeeds.
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "pizza", Name: "Margherita Pizza", PriceCents: 1299, Available: true,
	}, nil)
	// Second item is unavailable.
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "soda", Name: "Cola", PriceCents: 299, Available: false,
	}, nil)

	_, err := PlaceOrder(env.H(), "user_1", "rest_1",
		[]OrderItem{
			{SKU: "pizza", Quantity: 1},
			{SKU: "soda", Quantity: 2},
		},
		DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
	)

	if err == nil {
		t.Fatal("expected error for unavailable item, got nil")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("expected 'unavailable' error, got: %v", err)
	}
}

func TestPlaceOrder_PaymentFailureTriggersCompensation(t *testing.T) {
	env := setupEnv()

	// Menu lookups succeed.
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "pizza", Name: "Margherita Pizza", PriceCents: 1299, Available: true,
	}, nil)

	// Payment fails.
	env.OnCall("payments", "Charge", nil).Return("",
		fmt.Errorf("insufficient funds"))

	_, err := PlaceOrder(env.H(), "user_1", "rest_1",
		[]OrderItem{{SKU: "pizza", Quantity: 1}},
		DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
	)

	if err == nil {
		t.Fatal("expected error for payment failure, got nil")
	}
	if !strings.Contains(err.Error(), "payment") {
		t.Errorf("expected 'payment' error, got: %v", err)
	}
	// No driver should have been called.
	env.AssertNotCalled(t, "dispatch", "FindDriver")
}

func TestPlaceOrder_PickupTimeoutCompensates(t *testing.T) {
	env := setupEnv()

	// Menu lookups.
	env.OnCall("menu", "LookupItem", nil).ReturnJSON(menuItem{
		SKU: "pizza", Name: "Margherita Pizza", PriceCents: 1299, Available: true,
	}, nil)

	// Payment succeeds.
	env.OnCall("payments", "Charge", nil).ReturnJSON(paymentChargeResponse{
		ChargeID: "chg_abc123", Status: "ok",
	}, nil)

	// Dispatch succeeds.
	env.OnCall("dispatch", "FindDriver", nil).ReturnJSON(struct {
		DriverID   string `json:"driver_id"`
		DriverName string `json:"driver_name"`
		ETAMinutes int    `json:"eta_minutes"`
	}{DriverID: "drv_xyz", DriverName: "Alex", ETAMinutes: 15}, nil)

	// Driver accepts immediately.
	env.Signal("driver_accepted", `{"driver_name":"Alex"}`)

	// Restaurant notification.
	env.OnCall("restaurant", "NotifyOrder", nil).Return("{}", nil)

	// Compensation stubs.
	env.OnCall("dispatch", "ReleaseDriver", nil).Return("{}", nil)
	env.OnCall("payments", "Refund", nil).Return("{}", nil)

	// Run the workflow in a goroutine and advance time past the
	// 30-minute AwaitSignals timeout to trigger the timeout path.
	type outcome struct {
		result OrderResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, e := PlaceOrder(env.H(), "user_1", "rest_1",
			[]OrderItem{{SKU: "pizza", Quantity: 1}},
			DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
		)
		ch <- outcome{r, e}
	}()

	// Give the workflow time to reach the AwaitSignals call.
	time.Sleep(100 * time.Millisecond)

	// Advance simulated time past the 30-minute pickup timeout.
	env.AdvanceTime(31 * time.Minute)

	o := <-ch
	if o.err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(o.err.Error(), "timed out") {
		t.Errorf("expected 'timed out' error, got: %v", o.err)
	}

	// Compensation calls should have been made.
	env.AssertCalled(t, "dispatch", "ReleaseDriver")
	env.AssertCalled(t, "payments", "Refund")
}
