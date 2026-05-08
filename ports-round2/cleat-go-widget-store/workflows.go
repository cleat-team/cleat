package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/rcownie/cleat/cleat"
)

const PAYMENT_STATUS = "payment_status"
const PAYMENT_ID = "payment_id"
const ORDER_ID = "order_id"

// checkoutWorkflow implements the e-commerce checkout saga.
//  1. Create a new order in PENDING status
//  2. Reserve inventory (subtract 1 from widget stock)
//  3. Publish the payment ID externally via query state
//  4. Await the PAYMENT_STATUS signal (60s timeout)
//  5. On success: mark order PAID, start dispatchOrder child workflow
//     On failure/timeout: undo inventory reservation, cancel order
//  6. Publish the order ID externally via query state
func checkoutWorkflow(h cleat.HostCalls, _ string) (string, error) {
	workflowID := h.WorkflowID()
	if workflowID == "" {
		return "", fmt.Errorf("workflow ID is empty")
	}

	// Step 1: Create a new order
	var orderID int
	if err := h.DurableCallTyped("store", "createOrder", &struct{}{}, &orderID); err != nil {
		return "", fmt.Errorf("create order: %w", err)
	}

	// Step 2: Reserve inventory
	var reserved bool
	if err := h.DurableCallTyped("store", "reserveInventory", WIDGET_ID, &reserved); err != nil {
		return "", fmt.Errorf("reserve inventory: %w", err)
	}
	if !reserved {
		// No inventory available -- cancel the order
		h.DurableCallTyped("store", "updateOrderStatus", UpdateOrderStatusInput{
			OrderID: orderID, OrderStatus: CANCELLED,
		}, nil)
		h.SetQueryState(PAYMENT_ID, "")
		return "", nil
	}

	// Publish payment ID for the HTTP handler to retrieve
	h.SetQueryState(PAYMENT_ID, workflowID)

	// Step 3: Await payment signal (60s timeout)
	result := h.AwaitSignals([]string{PAYMENT_STATUS}, 60*time.Second)
	if result.TimedOut || result.Payload != "paid" {
		// Payment failed -- compensate: undo reservation and cancel order
		h.DurableCallTyped("store", "undoReserveInventory", WIDGET_ID, nil)
		h.DurableCallTyped("store", "updateOrderStatus", UpdateOrderStatusInput{
			OrderID: orderID, OrderStatus: CANCELLED,
		}, nil)
		h.SetQueryState(ORDER_ID, strconv.Itoa(orderID))
		return "", nil
	}

	// Payment succeeded -- mark as paid
	h.DurableCallTyped("store", "updateOrderStatus", UpdateOrderStatusInput{
		OrderID: orderID, OrderStatus: PAID,
	}, nil)

	// Start dispatch child workflow (fire-and-forget)
	h.ChildWorkflow("dispatchOrder", strconv.Itoa(orderID))

	h.SetQueryState(ORDER_ID, strconv.Itoa(orderID))
	return "", nil
}

// dispatchOrderWorkflow simulates order fulfillment over 10 seconds.
// Updates progress each second, dispatching the order when progress reaches 0.
func dispatchOrderWorkflow(h cleat.HostCalls, orderIDStr string) (string, error) {
	for range 10 {
		h.DurableSleep(1 * time.Second)
		if _, err := h.DurableCall("store", "updateOrderProgress", orderIDStr); err != nil {
			return "", fmt.Errorf("update progress: %w", err)
		}
	}
	return "", nil
}
