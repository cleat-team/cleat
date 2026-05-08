package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rcownie/cleat/durable/cleattest"
)

// ---------------------------------------------------------------------------
// Shop Service Tests
// ---------------------------------------------------------------------------

func TestSubtractInventory(t *testing.T) {
	shop := NewShopService()

	resp, err := shop.subtractInventory(`{"product":"widget","quantity":3}`)
	if err != nil {
		t.Fatalf("subtractInventory failed: %v", err)
	}
	if resp != `{"ok":true}` {
		t.Errorf("expected ok, got %s", resp)
	}

	shop.mu.Lock()
	qty := shop.inventory["widget"]
	shop.mu.Unlock()
	if qty != 97 {
		t.Errorf("expected inventory 97, got %d", qty)
	}
}

func TestSubtractInventoryInsufficient(t *testing.T) {
	shop := NewShopService()

	_, err := shop.subtractInventory(`{"product":"widget","quantity":999}`)
	if err == nil {
		t.Fatal("expected error for insufficient inventory")
	}
	if err.Error() != "insufficient inventory: have 100, need 999" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSubtractInventoryUnknownProduct(t *testing.T) {
	shop := NewShopService()

	_, err := shop.subtractInventory(`{"product":"nonexistent","quantity":1}`)
	if err == nil {
		t.Fatal("expected error for unknown product")
	}
}

func TestUndoSubtractInventory(t *testing.T) {
	shop := NewShopService()

	shop.subtractInventory(`{"product":"widget","quantity":5}`)
	shop.undoSubtractInventory(`{"product":"widget","quantity":5}`)

	shop.mu.Lock()
	qty := shop.inventory["widget"]
	shop.mu.Unlock()
	if qty != 100 {
		t.Errorf("expected inventory restored to 100, got %d", qty)
	}
}

func TestCreateOrder(t *testing.T) {
	shop := NewShopService()

	resp, err := shop.createOrder(`{"product":"widget","quantity":1}`)
	if err != nil {
		t.Fatalf("createOrder failed: %v", err)
	}

	var result struct {
		OrderID   string `json:"order_id"`
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if result.OrderID == "" || result.PaymentID == "" {
		t.Errorf("expected non-empty order_id and payment_id, got %+v", result)
	}
}

func TestMarkOrderPaid(t *testing.T) {
	shop := NewShopService()

	resp, _ := shop.createOrder(`{"product":"widget","quantity":1}`)
	var orderInfo struct {
		OrderID string `json:"order_id"`
	}
	json.Unmarshal([]byte(resp), &orderInfo)

	_, err := shop.markOrderPaid(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
	if err != nil {
		t.Fatalf("markOrderPaid failed: %v", err)
	}

	orderResp, _ := shop.retrieveOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
	var order Order
	json.Unmarshal([]byte(orderResp), &order)
	if order.Status != "paid" {
		t.Errorf("expected status 'paid', got %q", order.Status)
	}
}

func TestErrorOrder(t *testing.T) {
	shop := NewShopService()

	resp, _ := shop.createOrder(`{"product":"gadget","quantity":1}`)
	var orderInfo struct {
		OrderID string `json:"order_id"`
	}
	json.Unmarshal([]byte(resp), &orderInfo)

	shop.errorOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))

	orderResp, _ := shop.retrieveOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
	var order Order
	json.Unmarshal([]byte(orderResp), &order)
	if order.Status != "error" {
		t.Errorf("expected status 'error', got %q", order.Status)
	}
}

func TestUpdateOrderProgress(t *testing.T) {
	shop := NewShopService()

	resp, _ := shop.createOrder(`{"product":"widget","quantity":1}`)
	var orderInfo struct {
		OrderID string `json:"order_id"`
	}
	json.Unmarshal([]byte(resp), &orderInfo)

	shop.updateOrderProgress(fmt.Sprintf(`{"order_id":"%s","progress":"step 1/10"}`, orderInfo.OrderID))

	orderResp, _ := shop.retrieveOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
	var order Order
	json.Unmarshal([]byte(orderResp), &order)
	if order.Progress != "step 1/10" {
		t.Errorf("expected progress 'step 1/10', got %q", order.Progress)
	}
}

// ---------------------------------------------------------------------------
// Workflow Tests (using cleattest.TestEnv)
// ---------------------------------------------------------------------------

func TestCheckoutWorkflow_Success(t *testing.T) {
	env := cleattest.NewTestEnv()
	shop := NewShopService()

	env.OnCall("shop", "subtractInventory", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	env.OnCall("shop", "createOrder", nil).ReturnJSON(map[string]string{
		"order_id":   "order-00001",
		"payment_id": "pay-00001",
	}, nil)
	env.OnCall("shop", "markOrderPaid", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	env.OnCall("shop", "updateOrderProgress", nil).ReturnJSON(map[string]bool{"ok": true}, nil)

	// Pre-signal the payment so AwaitSignals returns immediately
	env.Signal("PAYMENT_TOPIC", `{"order_id":"order-00001","transaction_id":"txn_abc"}`)

	h := env.H()

	inputJSON := `{"product":"widget","quantity":1}`
	result := runCheckoutWorkflow(h, shop, inputJSON)
	t.Logf("Result: %s", result)

	var resultObj struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
		Error   string `json:"error,omitempty"`
	}
	json.Unmarshal([]byte(result), &resultObj)

	if resultObj.Error != "" {
		t.Fatalf("workflow failed: %s", resultObj.Error)
	}
	if resultObj.OrderID == "" {
		t.Fatal("expected non-empty order_id")
	}
	if resultObj.Status != "paid" {
		t.Errorf("expected status 'paid', got %q", resultObj.Status)
	}

	env.AssertCalled(t, "shop", "subtractInventory")
	env.AssertCalled(t, "shop", "createOrder")
	env.AssertCalled(t, "shop", "markOrderPaid")
}

func TestCheckoutWorkflow_InventoryFailure(t *testing.T) {
	env := cleattest.NewTestEnv()
	shop := NewShopService()

	env.OnCall("shop", "subtractInventory", nil).Return("", fmt.Errorf("insufficient inventory"))

	h := env.H()

	inputJSON := `{"product":"widget","quantity":999}`
	result := runCheckoutWorkflow(h, shop, inputJSON)
	t.Logf("Result: %s", result)

	var resultObj struct {
		Error string `json:"error"`
	}
	json.Unmarshal([]byte(result), &resultObj)

	if resultObj.Error == "" {
		t.Fatal("expected error for inventory failure")
	}

	env.AssertCalled(t, "shop", "subtractInventory")
	env.AssertNotCalled(t, "shop", "createOrder")
}

func TestCheckoutWorkflow_PaymentTimeout(t *testing.T) {
	env := cleattest.NewTestEnv()
	shop := NewShopService()

	env.OnCall("shop", "subtractInventory", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	env.OnCall("shop", "createOrder", nil).ReturnJSON(map[string]string{
		"order_id":   "order-00001",
		"payment_id": "pay-00001",
	}, nil)
	env.OnCall("shop", "errorOrder", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	env.OnCall("shop", "undoSubtractInventory", nil).ReturnJSON(map[string]bool{"ok": true}, nil)

	h := env.H()

	inputJSON := `{"product":"widget","quantity":1}`

	type wfResult struct {
		result string
	}
	ch := make(chan wfResult, 1)
	go func() {
		r := runCheckoutWorkflow(h, shop, inputJSON)
		ch <- wfResult{r}
	}()

	time.Sleep(100 * time.Millisecond)
	env.AdvanceTime(121 * time.Second)

	wr := <-ch

	var resultObj struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	json.Unmarshal([]byte(wr.result), &resultObj)

	if resultObj.Status != "payment_timeout" {
		t.Errorf("expected status 'payment_timeout', got %q", resultObj.Status)
	}

	env.AssertCalled(t, "shop", "errorOrder")
	env.AssertCalled(t, "shop", "undoSubtractInventory")
}

func TestCheckoutWorkflow_FullFlow(t *testing.T) {
	env := cleattest.NewTestEnv()
	shop := NewShopService()

	env.OnCall("shop", "subtractInventory", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	env.OnCall("shop", "createOrder", nil).ReturnJSON(map[string]string{
		"order_id":   "order-00001",
		"payment_id": "pay-00001",
	}, nil)
	env.OnCall("shop", "markOrderPaid", nil).ReturnJSON(map[string]bool{"ok": true}, nil)

	h := env.H()

	inputJSON := `{"product":"widget","quantity":1}`

	type wfResult struct {
		result string
	}
	ch := make(chan wfResult, 1)
	go func() {
		r := runCheckoutWorkflow(h, shop, inputJSON)
		ch <- wfResult{r}
	}()

	time.Sleep(100 * time.Millisecond)
	env.Signal("PAYMENT_TOPIC", `{"order_id":"order-00001","transaction_id":"txn_abc"}`)

	wr := <-ch

	var resultObj struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	json.Unmarshal([]byte(wr.result), &resultObj)

	if resultObj.Status != "paid" {
		t.Errorf("expected status 'paid', got %q", resultObj.Status)
	}
	if resultObj.OrderID != "order-00001" {
		t.Errorf("expected order_id 'order-00001', got %q", resultObj.OrderID)
	}

	env.AssertCalled(t, "shop", "subtractInventory")
	env.AssertCalled(t, "shop", "createOrder")
	env.AssertCalled(t, "shop", "markOrderPaid")
}

// ---------------------------------------------------------------------------
// Dispatch workflow tests
// ---------------------------------------------------------------------------

func TestDispatchWorkflow(t *testing.T) {
	shop := NewShopService()

	resp, _ := shop.createOrder(`{"product":"widget","quantity":1}`)
	var orderInfo struct {
		OrderID string `json:"order_id"`
	}
	json.Unmarshal([]byte(resp), &orderInfo)

	env := cleattest.NewTestEnv()

	for i := 0; i < 10; i++ {
		env.OnCall("shop", "updateOrderProgress", nil).ReturnJSON(map[string]bool{"ok": true}, nil)
	}

	h := env.H()

	type dispatchResult struct {
		result string
	}
	ch := make(chan dispatchResult, 1)
	go func() {
		r := runDispatchWorkflow(h, shop, fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
		ch <- dispatchResult{r}
	}()

	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		env.AdvanceTime(1100 * time.Millisecond)
	}

	dr := <-ch
	t.Logf("Dispatch result: %s", dr.result)

	var resultObj struct {
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	json.Unmarshal([]byte(dr.result), &resultObj)

	if resultObj.Status != "delivered" {
		t.Errorf("expected status 'delivered', got %q", resultObj.Status)
	}
}

// ---------------------------------------------------------------------------
// Integration test: Full checkout through shop service
// ---------------------------------------------------------------------------

func TestShopService_FullIntegration(t *testing.T) {
	shop := NewShopService()

	shop.mu.Lock()
	widgetQty := shop.inventory["widget"]
	shop.mu.Unlock()
	if widgetQty != 100 {
		t.Errorf("expected 100 widgets, got %d", widgetQty)
	}

	_, err := shop.subtractInventory(`{"product":"widget","quantity":1}`)
	if err != nil {
		t.Fatalf("subtract failed: %v", err)
	}

	resp, err := shop.createOrder(`{"product":"widget","quantity":1}`)
	if err != nil {
		t.Fatalf("create order failed: %v", err)
	}
	var orderInfo struct {
		OrderID   string `json:"order_id"`
		PaymentID string `json:"payment_id"`
	}
	json.Unmarshal([]byte(resp), &orderInfo)

	shop.markOrderPaid(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))

	for i := 0; i < 10; i++ {
		shop.updateOrderProgress(fmt.Sprintf(`{"order_id":"%s","progress":"step %d/10"}`, orderInfo.OrderID, i+1))
	}

	orderResp, _ := shop.retrieveOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
	var order Order
	json.Unmarshal([]byte(orderResp), &order)

	if order.Status != "paid" {
		t.Errorf("expected 'paid', got %q", order.Status)
	}
	if order.Progress != "step 10/10" {
		t.Errorf("expected 'step 10/10', got %q", order.Progress)
	}

	shop.mu.Lock()
	widgetQty = shop.inventory["widget"]
	shop.mu.Unlock()
	if widgetQty != 99 {
		t.Errorf("expected 99 widgets, got %d", widgetQty)
	}
}
