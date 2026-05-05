// Package restaurant_spec defines types and the Client interface for the restaurant service.
package restaurant_spec

type NotifyOrderRequest struct {
	RestaurantID string `json:"restaurant_id"`
	Items        string `json:"items"`
	ETAMinutes   int    `json:"eta_minutes"`
}

type CancelOrderRequest struct {
	OrderID string `json:"order_id"`
}

// Client is the typed client interface for the restaurant service.
type Client interface {
	NotifyOrder(req NotifyOrderRequest) error
	CancelOrder(req CancelOrderRequest) error
}
