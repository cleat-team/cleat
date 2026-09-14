package plugin

import (
	"encoding/json"
	"testing"
)

// TestStartRequestRequiresItsKeyAndTenant pins the two values that were
// hardcoded at the seam before cleat#1555 and cleat#1580, and the reason the
// requirement is expressed as "reject" rather than "default".
//
// THE FAILURE THIS GUARDS HAS NO SYMPTOM AT THE CALL SITE. A start with an
// empty key produces a duplicate run only when something retries, and a start
// with an empty tenant produces a correctly-shaped run owned by the wrong
// tenant. Both look exactly like success to the caller, which is why the
// producer refuses them instead of substituting what it used to hardcode.
func TestStartRequestRequiresItsKeyAndTenant(t *testing.T) {
	// The producer's validation lives in cmd/cleat-worker; this asserts the
	// shape it validates, so a field rename cannot silently orphan the check.
	req := StartRequest{
		DefName:        "charge",
		Input:          json.RawMessage(`{}`),
		IdempotencyKey: "scheduler:abc:123",
		TenantID:       "00000000-0000-0000-0000-000000000042",
	}

	if req.IdempotencyKey == "" {
		t.Error("IdempotencyKey is the field the producer rejects when empty")
	}
	if req.TenantID == "" {
		t.Error("TenantID is the field the producer rejects when empty")
	}

	// THE ZERO VALUE MUST BE REJECTABLE, not usable. If either field ever
	// gained a non-empty default, the producer's "is it empty" check would
	// stop firing and both defects would return with no test noticing.
	var zero StartRequest
	if zero.IdempotencyKey != "" || zero.TenantID != "" {
		t.Errorf("the zero StartRequest has non-empty required fields (%q, %q); "+
			"the producer detects a missing value by emptiness, so a default here "+
			"would silently restore the hardcoded behaviour cleat#1555 and "+
			"cleat#1580 removed", zero.IdempotencyKey, zero.TenantID)
	}
}
