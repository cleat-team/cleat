// Package dispatch_spec defines types and the Client interface for the dispatch service.
package dispatch_spec

type FindDriverRequest struct {
	Address string `json:"address"`
}

type FindDriverResponse struct {
	DriverID string `json:"driver_id"`
}

type ReleaseDriverRequest struct {
	DriverID string `json:"driver_id"`
}

type CheckPickupStatusRequest struct {
	DriverID string `json:"driver_id"`
}

type CheckPickupStatusResponse struct {
	Status string `json:"status"`
}

// Client is the typed client interface for the dispatch service.
type Client interface {
	FindDriver(req FindDriverRequest) (*FindDriverResponse, error)
	ReleaseDriver(req ReleaseDriverRequest) error
	CheckPickupStatus(req CheckPickupStatusRequest) (*CheckPickupStatusResponse, error)
}
