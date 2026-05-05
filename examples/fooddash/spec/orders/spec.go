// Package orders_spec defines types and the Client interface for the orders service.
package orders_spec

type GetStateRequest struct {
	OrderID string `json:"order_id"`
}

type GetStateResponse struct {
	Status     string `json:"status"`
	DriverName string `json:"driver_name"`
	ETAMinutes int    `json:"eta_minutes"`
	TotalCents int    `json:"total_cents"`
}

// Client is the typed client interface for the orders service.
type Client interface {
	GetState(req GetStateRequest) (*GetStateResponse, error)
}
