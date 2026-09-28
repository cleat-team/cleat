// normalize-order is the LEGITIMATE tenant step in the wedge scenario: a
// stand-in for the "normalize-order" transform docs/playbooks/integration-hub.md
// describes a tenant uploading through POST /api/definitions. It does nothing
// host-privileged -- no file, no network, no environment variable -- because
// an ordinary tenant transform needs none of the access the sandbox refuses.
// It exists to show the wedge's HAPPY path: a tenant's own code, running as a
// child of the hub's workflow, doing real work.
package normalizeorder

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

type rawEvent struct {
	OrderID    string          `json:"order_id"`
	VendorName string          `json:"vendor_name"`
	Extra      json.RawMessage `json:"-"`
}

// NormalizeOrder reshapes a vendor-specific event into the shape the hub's own
// SyncCustomer expects downstream. THE INPUT IS A STRING, matching
// examples/integration-hub/hub.go's SyncCustomer and examples/order-lifecycle's
// convention: an entry point with exactly one string parameter receives the
// whole input JSON verbatim (wasm/exports.go; cleat#824).
func NormalizeOrder(h cleat.HostCalls, input string) (string, error) {
	var in rawEvent
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode raw event: %w", err)
	}
	if in.OrderID == "" {
		return "", fmt.Errorf("order_id is required")
	}

	h.Log("normalizing order", "order_id", in.OrderID, "vendor", in.VendorName)

	out := map[string]any{
		"order_id":    in.OrderID,
		"normalized":  true,
		"transformed": "normalize-order v1",
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
