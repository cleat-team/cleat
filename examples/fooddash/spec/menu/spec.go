// Package menu_spec defines types and the Client interface for the menu service.
package menu_spec

type LookupItemRequest struct {
	RestaurantID string `json:"restaurant_id"`
	SKU          string `json:"sku"`
}

type LookupItemResponse struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	PriceCents int    `json:"price_cents"`
	Available  bool   `json:"available"`
}

// Client is the typed client interface for the menu service.
type Client interface {
	LookupItem(req LookupItemRequest) (*LookupItemResponse, error)
}
