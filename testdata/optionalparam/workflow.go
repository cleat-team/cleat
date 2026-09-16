// Package optionalparam is the fixture for cleat#1065's optional-parameter
// mechanism: a POINTER entry-point parameter means "may be absent".
//
// Before this, Go had no way to say that. A string or an int bound its zero
// value when the key was missing, which is not the same statement -- it cannot
// tell "the caller sent zero" from "the caller sent nothing" -- and every other
// type, pointers included, was a hard bind error on absence. So "optional" was
// expressible only by accident, and only for two types.
//
// The fixture carries both arms of the distinction on purpose:
//
//	promo *Coupon  absent -> nil, and the workflow RUNS
//	userID string   absent -> "",  the pre-existing zero-binding, unchanged
//
// The string is the control. Without it, a run that bound nothing at all would
// look identical to a run that correctly bound an absent optional -- the
// workflow would return its "no coupon" answer either way.
package optionalparam

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Coupon is a composite, which is the case that mattered: an absent composite
// is a bind error in Go, so *Coupon is the only way to declare one optional.
type Coupon struct {
	Code     string `json:"code"`
	OffCents int    `json:"off_cents"`
}

//cleat:entry
func ApplyCoupon(h cleat.HostCalls, userID string, promo *Coupon) (string, error) {
	if promo == nil {
		return fmt.Sprintf(`{"user":%q,"coupon":null}`, userID), nil
	}
	return fmt.Sprintf(`{"user":%q,"coupon":%q,"off":%d}`,
		userID, promo.Code, promo.OffCents), nil
}
