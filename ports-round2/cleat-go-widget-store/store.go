package main

import (
	"context"
	"encoding/json"
)

// StoreClient provides database access through the Cleat HostCalls interface.
// In a real deployment these would invoke backing services; in tests they
// are stubbed via durabletest.TestEnv.OnCall.
type StoreClient struct {
	ctx context.Context
}

// NewStoreClient creates a StoreClient for real database access (non-workflow code).
func NewStoreClient(ctx context.Context) *StoreClient {
	return &StoreClient{ctx: ctx}
}

// ReserveInventory decrements inventory by 1 atomically.
// Returns true if inventory was available and reserved.
func (s *StoreClient) ReserveInventory() (bool, error) {
	result, err := db.Exec(s.ctx,
		"UPDATE products SET inventory = inventory - 1 WHERE product_id = $1 AND inventory > 0",
		WIDGET_ID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

// UndoReserveInventory increments inventory by 1 (compensation for reserveInventory).
func (s *StoreClient) UndoReserveInventory() error {
	_, err := db.Exec(s.ctx,
		"UPDATE products SET inventory = inventory + 1 WHERE product_id = $1",
		WIDGET_ID)
	return err
}

// CreateOrder inserts a new order with PENDING status and returns its ID.
func (s *StoreClient) CreateOrder() (int, error) {
	var orderID int
	err := db.QueryRow(s.ctx,
		"INSERT INTO orders (order_status) VALUES ($1) RETURNING order_id",
		int(PENDING)).Scan(&orderID)
	return orderID, err
}

// UpdateOrderStatus sets the status of an order.
func (s *StoreClient) UpdateOrderStatus(input UpdateOrderStatusInput) error {
	_, err := db.Exec(s.ctx,
		"UPDATE orders SET order_status = $1 WHERE order_id = $2",
		int(input.OrderStatus), input.OrderID)
	return err
}

// UpdateOrderProgress decrements progress_remaining by 1.
// Returns the remaining progress. When it reaches 0, marks the order DISPATCHED.
func (s *StoreClient) UpdateOrderProgress(orderID int) (int, error) {
	var progressRemaining int
	err := db.QueryRow(s.ctx,
		"UPDATE orders SET progress_remaining = progress_remaining - 1 WHERE order_id = $1 RETURNING progress_remaining",
		orderID).Scan(&progressRemaining)
	if err != nil {
		return 0, err
	}
	if progressRemaining == 0 {
		if err := s.UpdateOrderStatus(UpdateOrderStatusInput{OrderID: orderID, OrderStatus: DISPATCHED}); err != nil {
			return 0, err
		}
	}
	return progressRemaining, nil
}

// ---------------------------------------------------------------------------
// JSON helpers for workflow-step serialization used in DurableCall stubs
// ---------------------------------------------------------------------------

// ReserveInventoryOutput is the JSON output of a reserveInventory call.
type ReserveInventoryOutput struct {
	Success bool `json:"success"`
}

// UpdateOrderProgressOutput is the JSON output of an updateOrderProgress call.
type UpdateOrderProgressOutput struct {
	ProgressRemaining int `json:"progress_remaining"`
}

// MarshalCreateOrderRequest marshals the create-order request.
func MarshalCreateOrderRequest() (string, error) {
	data, err := json.Marshal(struct{}{})
	return string(data), err
}

// MarshalReserveInventoryRequest marshals the reserve-inventory request.
func MarshalReserveInventoryRequest() (string, error) {
	data, err := json.Marshal(WIDGET_ID)
	return string(data), err
}

// MarshalUpdateOrderStatusRequest marshals an update-order-status request.
func MarshalUpdateOrderStatusRequest(input UpdateOrderStatusInput) (string, error) {
	data, err := json.Marshal(input)
	return string(data), err
}
