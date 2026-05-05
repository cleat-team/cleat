// Package payments_spec defines types and the Client interface for the payments service.
package payments_spec

type ChargeRequest struct {
	UserID      string `json:"user_id"`
	AmountCents int    `json:"amount_cents"`
	Currency    string `json:"currency"`
}

type ChargeResponse struct {
	ChargeID string `json:"charge_id"`
	Status   string `json:"status"`
}

type RefundRequest struct {
	ChargeID string `json:"charge_id"`
}

// Client is the typed client interface for the payments service.
type Client interface {
	Charge(req ChargeRequest) (*ChargeResponse, error)
	Refund(req RefundRequest) error
}
